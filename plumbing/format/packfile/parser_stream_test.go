package packfile_test

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	packutil "github.com/go-git/go-git/v6/plumbing/format/packfile/util"
	"github.com/go-git/go-git/v6/storage/memory"
)

type discardPackStorage struct {
	*memory.Storage
	base       plumbing.EncodedObject
	failure    error
	largest    int
	writes     int
	highMemory bool
}

func (s *discardPackStorage) LowMemoryMode() bool { return !s.highMemory }
func (s *discardPackStorage) EncodedObject(plumbing.ObjectType, plumbing.Hash) (plumbing.EncodedObject, error) {
	return s.base, nil
}

func (s *discardPackStorage) RawObjectWriter(plumbing.ObjectType, int64) (io.WriteCloser, error) {
	s.writes++
	if s.failure != nil {
		return nil, s.failure
	}
	return discardPackWriter{largest: &s.largest}, nil
}

type discardPackWriter struct{ largest *int }

func (w discardPackWriter) Write(p []byte) (int, error) {
	*w.largest = max(*w.largest, len(p))
	return len(p), nil
}
func (discardPackWriter) Close() error { return nil }
func BenchmarkParserLowMemoryDelta(b *testing.B) {
	const size = 16 << 20
	base := &plumbing.MemoryObject{}
	base.SetType(plumbing.BlobObject)
	_, err := base.Write(make([]byte, size))
	require.NoError(b, err)
	delta := append(packutil.EncodeLEB128(size), packutil.EncodeLEB128(size)...)
	delta = append(delta, 0xc0, 0x80, 0xc0, 0x80)
	packed := thinDeltaPack(b, base.Hash(), delta)
	storage := &discardPackStorage{Storage: memory.NewStorage(), base: base}
	runtime.GC()
	runtime.GC()
	b.ReportAllocs()
	b.SetBytes(size)
	b.ResetTimer()
	for b.Loop() {
		_, err := packfile.NewParser(bytes.NewReader(packed), packfile.WithStorage(storage)).Parse()
		require.NoError(b, err)
	}
}

func thinDeltaPack(tb testing.TB, h plumbing.Hash, delta []byte) []byte {
	tb.Helper()
	var packed bytes.Buffer
	digest := sha1.New()
	out := io.MultiWriter(&packed, digest)
	_, _ = out.Write([]byte("PACK"))
	_ = binary.Write(out, binary.BigEndian, uint32(2))
	_ = binary.Write(out, binary.BigEndian, uint32(1))
	writePackObjectHeader(tb, out, plumbing.REFDeltaObject, int64(len(delta)))
	_, _ = h.WriteTo(out)
	writeZlibPayload(tb, out, delta)
	_, _ = packed.Write(digest.Sum(nil))
	return packed.Bytes()
}

type countedDeltaBase struct {
	plumbing.EncodedObject
	opens int
}

func (o *countedDeltaBase) Reader() (io.ReadCloser, error) {
	o.opens++
	return o.EncodedObject.Reader()
}

func TestLowMemoryParserAdmissionAndLength(t *testing.T) {
	t.Parallel()
	base := &plumbing.MemoryObject{}
	base.SetType(plumbing.BlobObject)
	_, err := base.Write([]byte("abc"))
	require.NoError(t, err)
	_ = base.Hash()
	for _, mode := range []string{"admission", "base limit", "target limit", "length"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			counted := &countedDeltaBase{EncodedObject: base}
			storage := &discardPackStorage{Storage: memory.NewStorage(), base: counted}
			delta := packfile.DiffDelta([]byte("abc"), []byte("ab"))
			limit := int64(3)
			injected := errors.New("write rejected")
			switch mode {
			case "admission":
				storage.failure = injected
			case "base limit":
				limit = 2
			case "target limit":
				delta = packfile.DiffDelta([]byte("abc"), []byte("abcd"))
			}
			packed := thinDeltaPack(t, base.Hash(), delta)
			p := packfile.NewParser(bytes.NewReader(append(packed, []byte("trailing")...)), packfile.WithStorage(storage), packfile.WithMaxObjectSize(limit))
			_, err := p.Parse()
			switch mode {
			case "length":
				require.NoError(t, err)
				require.Equal(t, int64(len(packed)), p.BytesRead())
			case "admission":
				require.ErrorIs(t, err, injected)
				require.Zero(t, counted.opens)
			default:
				require.ErrorIs(t, err, packfile.ErrObjectTooLarge)
				require.Zero(t, counted.opens)
			}
		})
	}
}

func TestLowMemoryParserStreamsWrites(t *testing.T) {
	t.Parallel()
	base := &plumbing.MemoryObject{}
	base.SetType(plumbing.BlobObject)
	_, err := base.Write([]byte("abc"))
	require.NoError(t, err)
	target := bytes.Repeat([]byte("x"), 1<<20)
	packed := thinDeltaPack(t, base.Hash(), packfile.DiffDelta([]byte("abc"), target))
	storage := &discardPackStorage{Storage: memory.NewStorage(), base: base}
	_, err = packfile.NewParser(bytes.NewReader(packed), packfile.WithStorage(storage)).Parse()
	require.NoError(t, err)
	require.Positive(t, storage.largest)
	require.LessOrEqual(t, storage.largest, 32<<10)
}

type objectAdmissionObserver struct {
	rejectingObserver
	check func(plumbing.ObjectType, int64, int64) error
}

func (o objectAdmissionObserver) OnInflatedObjectHeader(k plumbing.ObjectType, n, pos int64) error {
	return o.check(k, n, pos)
}

func TestParserObjectAdmissionBeforeIO(t *testing.T) {
	t.Parallel()
	for _, high := range []bool{false, true} {
		for _, delta := range []bool{false, true} {
			t.Run(fmt.Sprintf("high=%v/delta=%v", high, delta), func(t *testing.T) {
				t.Parallel()
				base := &plumbing.MemoryObject{}
				base.SetType(plumbing.BlobObject)
				_, err := base.Write([]byte("abc"))
				require.NoError(t, err)
				counted := &countedDeltaBase{EncodedObject: base}
				storage := &discardPackStorage{Storage: memory.NewStorage(), base: counted, highMemory: high}
				packed := buildAlternatingDeltaChainPack(t, 0)
				size := int64(len("benchmark base payload for the alternating delta chain"))
				if delta {
					packed = thinDeltaPack(t, base.Hash(), packfile.DiffDelta([]byte("abc"), []byte("ab")))
					size = 2
				}
				rejected := errors.New("metadata rejected")
				calls := 0
				observer := objectAdmissionObserver{check: func(k plumbing.ObjectType, n, pos int64) error {
					calls++
					require.Equal(t, plumbing.BlobObject, k)
					require.Equal(t, size, n)
					require.Equal(t, int64(12), pos)
					return rejected
				}}
				_, err = packfile.NewParser(bytes.NewReader(packed), packfile.WithStorage(storage), packfile.WithScannerObservers(&observer)).Parse()
				require.ErrorIs(t, err, rejected)
				require.Equal(t, 1, calls)
				require.Zero(t, storage.writes)
				require.Zero(t, counted.opens)
			})
		}
	}
}

func TestParserRejectsDeltaZlibChecksum(t *testing.T) {
	t.Parallel()
	base := &plumbing.MemoryObject{}
	base.SetType(plumbing.BlobObject)
	w, err := base.Writer()
	require.NoError(t, err)
	_, err = w.Write([]byte("abc"))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	packed := thinDeltaPack(t, base.Hash(), []byte{3, 1, 0x90, 1})
	packed[len(packed)-sha1.Size-1] ^= 1
	digest := sha1.Sum(packed[:len(packed)-sha1.Size])
	copy(packed[len(packed)-sha1.Size:], digest[:])
	storage := &discardPackStorage{Storage: memory.NewStorage(), base: base}
	_, err = packfile.NewParser(bytes.NewReader(packed), packfile.WithStorage(storage)).Parse()
	require.ErrorIs(t, err, zlib.ErrChecksum)
	require.Zero(t, storage.writes)
}
