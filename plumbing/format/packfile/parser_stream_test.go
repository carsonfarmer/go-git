package packfile_test

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	packutil "github.com/go-git/go-git/v6/plumbing/format/packfile/util"
	"github.com/go-git/go-git/v6/storage/memory"
	"github.com/stretchr/testify/require"
	"io"
	"runtime"
	"testing"
)

type discardPackStorage struct {
	*memory.Storage
	base    plumbing.EncodedObject
	failure error
	largest int
}

func (s *discardPackStorage) LowMemoryMode() bool { return true }
func (s *discardPackStorage) EncodedObject(plumbing.ObjectType, plumbing.Hash) (plumbing.EncodedObject, error) {
	return s.base, nil
}
func (s *discardPackStorage) RawObjectWriter(plumbing.ObjectType, int64) (io.WriteCloser, error) {
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
