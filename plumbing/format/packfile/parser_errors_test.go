package packfile_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/go-git/go-git/v6/storage/memory"
	"github.com/stretchr/testify/require"
)

type rejectingObserver struct {
	header  bool
	failure error
}

func (o *rejectingObserver) OnHeader(uint32) error {
	if o.header {
		return o.failure
	}
	return nil
}
func (o *rejectingObserver) OnInflatedObjectContent(plumbing.Hash, int64, uint32, []byte) error {
	return o.failure
}

func (o *rejectingObserver) OnInflatedObjectHeader(plumbing.ObjectType, int64, int64) error {
	return nil
}
func (o *rejectingObserver) OnFooter(plumbing.Hash) error { return nil }

type closeFailureStorage struct {
	*memory.Storage
	remaining int
	failure   error
}

func (s *closeFailureStorage) RawObjectWriter(kind plumbing.ObjectType, size int64) (io.WriteCloser, error) {
	w, err := s.Storage.RawObjectWriter(kind, size)
	if err != nil {
		return nil, err
	}
	s.remaining--
	if s.remaining == 0 {
		return &closeFailureWriter{WriteCloser: w, failure: s.failure}, nil
	}
	return w, nil
}

type closeFailureWriter struct {
	io.WriteCloser
	failure error
}

func (w *closeFailureWriter) Close() error { return errors.Join(w.WriteCloser.Close(), w.failure) }

func TestParserPropagatesAdmissionAndStorageErrors(t *testing.T) {
	t.Parallel()
	failure := errors.New("rejected by consumer")
	for _, header := range []bool{true, false} {
		t.Run(map[bool]string{true: "header", false: "content"}[header], func(t *testing.T) {
			t.Parallel()
			observer := &rejectingObserver{header: header, failure: failure}
			_, err := packfile.NewParser(bytes.NewReader(buildAlternatingDeltaChainPack(t, 0)), packfile.WithScannerObservers(observer)).Parse()
			require.ErrorIs(t, err, failure)
		})
	}
	for _, nth := range []int{1, 2} {
		t.Run(map[int]string{1: "plain close", 2: "delta close"}[nth], func(t *testing.T) {
			t.Parallel()
			storage := &closeFailureStorage{Storage: memory.NewStorage(), remaining: nth, failure: failure}
			_, err := packfile.NewParser(bytes.NewReader(buildAlternatingDeltaChainPack(t, 1)), packfile.WithStorage(storage)).Parse()
			require.ErrorIs(t, err, failure)
		})
	}
}
