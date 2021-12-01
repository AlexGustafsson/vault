package raft

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/golang/protobuf/proto"
	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	"github.com/hashicorp/vault/sdk/plugin/pb"
	"github.com/rboyer/safeio"
	"go.uber.org/atomic"
)

const (
	// pebbleSnapshotID is the stable ID for any pebble snapshot. Keeping the ID
	// stable means there is only ever one pebble snapshot in the system
	pebbleSnapshotID = "pebble-snapshot"
	tmpSuffix        = ".tmp"
	snapPath         = "snapshots"
)

// PebbleSnapshotStore implements the SnapshotStore interface and allows snapshots
// to be stored in pebble files on local disk. Since we always have an up to
// date FSM we use a special snapshot ID to indicate that the snapshot can be
// pulled from the pebble file that is currently backing the FSM. This allows us
// to provide just-in-time snapshots without doing incremental data dumps.
//
// When a snapshot is being installed on the node we will Create and Write data
// to it. This will cause the snapshot store to create a new pebble file and
// write the snapshot data to it. Then, we can simply rename the snapshot to the
// FSM's filename. This allows us to atomically install the snapshot and
// reduces the amount of disk i/o. Older snapshots are reaped on startup and
// before each subsequent snapshot write. This ensures we only have one snapshot
// on disk at a time.
type PebbleSnapshotStore struct {
	// path is the directory in which to store file based snapshots
	path string

	// We hold a copy of the FSM so we can stream snapshots straight out of the
	// database.
	fsm *FSM

	logger log.Logger
}

// PebbleSnapshotSink implements SnapshotSink optionally choosing to write to a
// file.
type PebbleSnapshotSink struct {
	store  *PebbleSnapshotStore
	logger log.Logger
	meta   raft.SnapshotMeta
	trans  raft.Transport

	// These fields will be used if we are writing a snapshot (vs. reading
	// one)
	written       atomic.Bool
	writer        io.WriteCloser
	writeError    error
	dir           string
	parentDir     string
	doneWritingCh chan struct{}

	l      sync.Mutex
	closed bool
}

// NewPebbleSnapshotStore creates a new PebbleSnapshotStore based
// on a base directory.
func NewPebbleSnapshotStore(base string, logger log.Logger, fsm *FSM) (*PebbleSnapshotStore, error) {
	if logger == nil {
		return nil, fmt.Errorf("no logger provided")
	}

	// Ensure our path exists
	path := filepath.Join(base, snapPath)
	if err := os.MkdirAll(path, 0o755); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("snapshot path not accessible: %v", err)
	}

	// Setup the store
	store := &PebbleSnapshotStore{
		logger: logger,
		fsm:    fsm,
		path:   path,
	}

	// Cleanup any old or failed snapshots on startup.
	if err := store.ReapSnapshots(); err != nil {
		return nil, err
	}

	return store, nil
}

// Create is used to start a new snapshot
func (f *PebbleSnapshotStore) Create(version raft.SnapshotVersion, index, term uint64, configuration raft.Configuration, configurationIndex uint64, trans raft.Transport) (raft.SnapshotSink, error) {
	// We only support version 1 snapshots at this time.
	if version != 1 {
		return nil, fmt.Errorf("unsupported snapshot version %d", version)
	}

	// Create the sink
	sink := &PebbleSnapshotSink{
		store:  f,
		logger: f.logger,
		meta: raft.SnapshotMeta{
			Version:            version,
			ID:                 pebbleSnapshotID,
			Index:              index,
			Term:               term,
			Configuration:      configuration,
			ConfigurationIndex: configurationIndex,
		},
		trans: trans,
	}

	return sink, nil
}

// List returns available snapshots in the store. It only returns pebble
// snapshots. No snapshot will be returned if there are no indexes in the
// FSM.
func (f *PebbleSnapshotStore) List() ([]*raft.SnapshotMeta, error) {
	meta, err := f.getMetaFromFSM()
	if err != nil {
		return nil, err
	}

	// If we haven't seen any data yet do not return a snapshot
	if meta.Index == 0 {
		return nil, nil
	}

	return []*raft.SnapshotMeta{meta}, nil
}

func (f *PebbleSnapshotStore) getMetaFromFSM() (*raft.SnapshotMeta, error) {
	latestIndex, latestConfig := f.fsm.LatestState()
	meta := &raft.SnapshotMeta{
		Version: 1,
		ID:      pebbleSnapshotID,
		Index:   latestIndex.Index,
		Term:    latestIndex.Term,
	}

	if latestConfig != nil {
		meta.ConfigurationIndex, meta.Configuration = protoConfigurationToRaftConfiguration(latestConfig)
	}

	return meta, nil
}

// Open takes a snapshot ID and returns a ReadCloser for that snapshot.
func (f *PebbleSnapshotStore) Open(id string) (*raft.SnapshotMeta, io.ReadCloser, error) {
	if id == pebbleSnapshotID {
		return f.openFromFSM()
	}

	return f.openFromDirectory(id)
}

func (f *PebbleSnapshotStore) openFromFSM() (*raft.SnapshotMeta, io.ReadCloser, error) {
	meta, err := f.getMetaFromFSM()
	if err != nil {
		return nil, nil, err
	}
	// If we don't have any data return an error
	if meta.Index == 0 {
		return nil, nil, errors.New("no snapshot data")
	}

	// Stream data out of the FSM to calculate the size
	readCloser, writeCloser := io.Pipe()
	metaReadCloser, metaWriteCloser := io.Pipe()
	go func() {
		f.fsm.writeTo(context.Background(), metaWriteCloser, writeCloser)
	}()

	// Compute the size
	n, err := io.Copy(ioutil.Discard, metaReadCloser)
	if err != nil {
		f.logger.Error("failed to read state file", "error", err)
		metaReadCloser.Close()
		readCloser.Close()
		return nil, nil, err
	}

	meta.Size = n
	metaReadCloser.Close()

	return meta, readCloser, nil
}

func (f *PebbleSnapshotStore) getMetaFromDB(id string) (*raft.SnapshotMeta, error) {
	if len(id) == 0 {
		return nil, errors.New("can not open empty snapshot ID")
	}

	dirname := filepath.Join(f.path, id, databaseDirectoryName)
	pebbleDB, err := pebble.Open(dirname, nil)
	if err != nil {
		return nil, err
	}
	defer pebbleDB.Close()

	meta := &raft.SnapshotMeta{
		Version: 1,
		ID:      id,
	}

	key := append(configBucketPrefix, latestIndexKey...)
	val, closer, err := pebbleDB.Get(key)
	if err != nil && err != pebble.ErrNotFound {
		if closer != nil {
			closer.Close()
		}

		return nil, err
	}

	if val != nil {
		var snapshotIndexes IndexValue
		err := proto.Unmarshal(val, &snapshotIndexes)
		if err != nil {
			return nil, err
		}

		meta.Index = snapshotIndexes.Index
		meta.Term = snapshotIndexes.Term
	}
	closer.Close()

	key = append(configBucketPrefix, latestConfigKey...)
	val, closer, err = pebbleDB.Get(key)
	if err != nil && err != pebble.ErrNotFound {
		if closer != nil {
			closer.Close()
		}

		return nil, err
	}

	if val != nil {
		var config ConfigurationValue
		err := proto.Unmarshal(val, &config)
		if err != nil {
			return nil, err
		}

		meta.ConfigurationIndex, meta.Configuration = protoConfigurationToRaftConfiguration(&config)
	}

	return meta, nil
}

func (f *PebbleSnapshotStore) openFromDirectory(id string) (*raft.SnapshotMeta, io.ReadCloser, error) {
	meta, err := f.getMetaFromDB(id)
	if err != nil {
		return nil, nil, err
	}

	dirname := filepath.Join(f.path, id, databaseDirectoryName)
	installer := &pebbleSnapshotInstaller{
		meta:       meta,
		ReadCloser: ioutil.NopCloser(strings.NewReader(dirname)),
		dirname:    dirname,
	}

	return meta, installer, nil
}

// ReapSnapshots reaps all snapshots.
func (f *PebbleSnapshotStore) ReapSnapshots() error {
	snapshots, err := ioutil.ReadDir(f.path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return nil
	default:
		f.logger.Error("failed to scan snapshot directory", "error", err)
		return err
	}

	for _, snap := range snapshots {
		// Ignore any files
		if !snap.IsDir() {
			continue
		}

		// Warn about temporary snapshots, this indicates a previously failed
		// snapshot attempt. We still want to clean these up.
		dirName := snap.Name()
		if strings.HasSuffix(dirName, tmpSuffix) {
			f.logger.Warn("found temporary snapshot", "name", dirName)
		}

		path := filepath.Join(f.path, dirName)
		f.logger.Info("reaping snapshot", "path", path)
		if err := os.RemoveAll(path); err != nil {
			f.logger.Error("failed to reap snapshot", "path", snap.Name(), "error", err)
			return err
		}
	}

	return nil
}

// ID returns the ID of the snapshot, can be used with Open()
// after the snapshot is finalized.
func (s *PebbleSnapshotSink) ID() string {
	s.l.Lock()
	defer s.l.Unlock()

	return s.meta.ID
}

func (s *PebbleSnapshotSink) writePebbleDatabase() error {
	// Create a new path
	name := snapshotName(s.meta.Term, s.meta.Index)
	path := filepath.Join(s.store.path, name+tmpSuffix)
	s.logger.Info("creating new snapshot", "path", path)

	// Make the directory
	if err := os.MkdirAll(path, 0o755); err != nil {
		s.logger.Error("failed to make snapshot directory", "error", err)
		return err
	}

	// Create the pebble database
	dbPath := filepath.Join(path, databaseDirectoryName)
	pebbleDB, err := pebble.Open(dbPath, nil)
	if err != nil {
		return err
	}

	// Write the snapshot metadata
	if err := writeSnapshotMetaToDB(&s.meta, pebbleDB); err != nil {
		return err
	}

	// Set the snapshot ID to the generated name.
	s.meta.ID = name

	// Create the done channel
	s.doneWritingCh = make(chan struct{})

	// Store the directories so we can commit the changes on success or abort
	// them on failure.
	s.dir = path
	s.parentDir = s.store.path

	// Create a pipe so we pipe writes into the go routine below.
	reader, writer := io.Pipe()
	s.writer = writer

	// Start a go routine in charge of piping data from the snapshot's Write
	// call to the delimtedreader and the pebble database.
	go func() {
		defer close(s.doneWritingCh)
		defer pebbleDB.Close()

		// The delimted reader will parse full proto messages from the snapshot
		// data.
		protoReader := NewDelimitedReader(reader, math.MaxInt32)
		defer protoReader.Close()

		var done bool
		var keys int
		var err error
		entry := new(pb.StorageEntry)

		for !done {
			bt := pebbleDB.NewIndexedBatch()

			// Commit in batches of 50k. Pebble does some stuff I don't fully
			// understand yet, but I don't have a reason for deviating from
			// what we did for bolt, so here we are.
			for i := 0; i < 50000; i++ {
				err = protoReader.ReadMsg(entry)
				if err != nil {
					if err == io.EOF {
						done = true
					}
				}

				key := append(dataBucketPrefix, []byte(entry.Key)...)
				err = bt.Set(key, entry.Value, nil)
				if err != nil {
					done = true
				}
				keys += 1
			}

			if err != nil {
				s.logger.Error("snapshot write: failed to write transaction", "error", err)
				s.writeError = err
				return
			}

			s.logger.Trace("snapshot write: writing keys", "num_written", keys)
			bt.Commit(nil)
		}
	}()

	return nil
}

// Write is used to append to the pebble database. The first call to write ensures we
// have the directory created.
func (s *PebbleSnapshotSink) Write(b []byte) (int, error) {
	s.l.Lock()
	defer s.l.Unlock()

	// If this is the first call to Write we need to setup the pebble database and
	// kickoff the pipeline write
	if previouslyWritten := s.written.Swap(true); !previouslyWritten {
		// Reap any old snapshots
		if err := s.store.ReapSnapshots(); err != nil {
			return 0, err
		}

		if err := s.writePebbleDatabase(); err != nil {
			return 0, err
		}
	}

	return s.writer.Write(b)
}

// Close is used to indicate a successful end.
func (s *PebbleSnapshotSink) Close() error {
	s.l.Lock()
	defer s.l.Unlock()

	// Make sure close is idempotent
	if s.closed {
		return nil
	}
	s.closed = true

	if s.writer != nil {
		s.writer.Close()
		<-s.doneWritingCh

		if s.writeError != nil {
			// If we encountered an error while writing then we should remove
			// the directory and return the error
			_ = os.RemoveAll(s.dir)
			return s.writeError
		}

		// Move the directory into place
		newPath := strings.TrimSuffix(s.dir, tmpSuffix)

		var err error
		if runtime.GOOS != "windows" {
			err = safeio.Rename(s.dir, newPath)
		} else {
			err = os.Rename(s.dir, newPath)
		}

		if err != nil {
			s.logger.Error("failed to move snapshot into place", "error", err)
			return err
		}
	}

	return nil
}

// Cancel is used to indicate an unsuccessful end.
func (s *PebbleSnapshotSink) Cancel() error {
	s.l.Lock()
	defer s.l.Unlock()

	// Make sure close is idempotent
	if s.closed {
		return nil
	}
	s.closed = true

	if s.writer != nil {
		s.writer.Close()
		<-s.doneWritingCh

		// Attempt to remove all artifacts
		return os.RemoveAll(s.dir)
	}

	return nil
}

type pebbleSnapshotInstaller struct {
	io.ReadCloser
	meta    *raft.SnapshotMeta
	dirname string
}

func (i *pebbleSnapshotInstaller) Dirname() string {
	return i.dirname
}

func (i *pebbleSnapshotInstaller) Metadata() *raft.SnapshotMeta {
	return i.meta
}

func (i *pebbleSnapshotInstaller) Install(dirname string) error {
	if len(i.dirname) == 0 {
		return errors.New("snapshot filename empty")
	}

	if len(dirname) == 0 {
		return errors.New("fsm filename empty")
	}

	// Rename the snapshot to the FSM location
	if runtime.GOOS != "windows" {
		return safeio.Rename(i.dirname, dirname)
	} else {
		return os.Rename(i.dirname, dirname)
	}
}

// snapshotName generates a name for the snapshot.
func snapshotName(term, index uint64) string {
	now := time.Now()
	msec := now.UnixNano() / int64(time.Millisecond)
	return fmt.Sprintf("%d-%d-%d", term, index, msec)
}
