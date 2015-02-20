// Copyright 2015 Google Inc. All Rights Reserved.
// Author: jacobsa@google.com (Aaron Jacobs)

package fs

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"sync"

	"github.com/jacobsa/gcloud/gcs"
	"golang.org/x/net/context"

	"bazil.org/fuse"
	fusefs "bazil.org/fuse/fs"
)

// A remote object's name and metadata, along with a local temporary file that
// contains its contents (when initialized).
//
// TODO(jacobsa): After becoming comfortable with the representation of dir and
// its concurrency protection, audit this file and make sure it is up to par.
type file struct {
	logger     *log.Logger
	bucket     gcs.Bucket
	objectName string
	size       uint64

	mu       sync.RWMutex
	tempFile *os.File // GUARDED_BY(mu)
}

// Make sure file implements the interfaces we think it does.
var (
	_ fusefs.Node           = &file{}
	_ fusefs.Handle         = &file{}
	_ fusefs.HandleReader   = &file{}
	_ fusefs.HandleReleaser = &file{}
	_ fusefs.HandleWriter   = &file{}
)

func newFile(
	logger *log.Logger,
	bucket gcs.Bucket,
	objectName string,
	size uint64) *file {
	return &file{
		logger:     logger,
		bucket:     bucket,
		objectName: objectName,
		size:       size,
	}
}

func (f *file) Attr() fuse.Attr {
	return fuse.Attr{
		// TODO(jacobsa): Expose ACLs from GCS?
		Mode: 0400,
		Size: f.size,
	}
}

// If the file contents have not yet been fetched to a temporary file, fetch
// them.
//
// LOCKS_EXCLUDED(f.mu)
func (f *file) ensureTempFile(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Do we already have a file?
	if f.tempFile != nil {
		return nil
	}

	// Create a temporary file.
	tempFile, err := ioutil.TempFile("", "gcsfuse")
	if err != nil {
		return fmt.Errorf("ioutil.TempFile: %v", err)
	}

	// Create a reader for the object.
	readCloser, err := f.bucket.NewReader(ctx, f.objectName)
	if err != nil {
		return fmt.Errorf("bucket.NewReader: %v", err)
	}

	defer readCloser.Close()

	// Copy the object contents into the file.
	if _, err := io.Copy(tempFile, readCloser); err != nil {
		return fmt.Errorf("io.Copy: %v", err)
	}

	// Save the file for later.
	f.tempFile = tempFile

	return nil
}

// Throw away the local temporary file, if any.
//
// LOCKS_EXCLUDED(f.mu)
func (f *file) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Is there a file to close?
	if f.tempFile == nil {
		return nil
	}

	// Close it, after grabbing its path.
	path := f.tempFile.Name()
	if err := f.tempFile.Close(); err != nil {
		f.logger.Println("Error closing temp file:", err)
	}

	// Attempt to delete it.
	if err := os.Remove(path); err != nil {
		f.logger.Println("Error deleting temp file:", err)
	}

	f.tempFile = nil
	return nil
}

// Ensure that the local temporary file is initialized, then read from it.
//
// LOCKS_EXCLUDED(f.mu)
func (f *file) Read(
	ctx context.Context,
	req *fuse.ReadRequest,
	resp *fuse.ReadResponse) error {
	// Ensure the temp file is present.
	if err := f.ensureTempFile(ctx); err != nil {
		return err
	}

	// Lock to read the temp file. If it went away in the meantime, that means
	// the kernel (erroneously) released us while reading from us.
	f.mu.RLock()
	defer f.mu.RUnlock()

	// Allocate a response buffer.
	resp.Data = make([]byte, req.Size)

	// Read the data.
	n, err := f.tempFile.ReadAt(resp.Data, req.Offset)
	resp.Data = resp.Data[:n]

	// Special case: read(2) doesn't return EOF errors.
	if err == io.EOF {
		err = nil
	}

	return err
}

// Ensure that the local temporary file is initialized, then write to it.
//
// LOCKS_EXCLUDED(f.mu)
func (f *file) Write(
	ctx context.Context,
	req *fuse.WriteRequest,
	resp *fuse.WriteResponse) error {
	return errors.New("TODO(jacobsa): Implement file.Write.")
}
