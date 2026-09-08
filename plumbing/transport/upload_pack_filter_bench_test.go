package transport

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/revlist"
	"github.com/go-git/go-git/v6/storage"
	"github.com/go-git/go-git/v6/storage/memory"
)

type selectionStorage struct {
	storage.Storer
	reads uint64
	sizes uint64
}

func (s *selectionStorage) EncodedObject(kind plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	s.reads++
	return s.Storer.EncodedObject(kind, h)
}

func (s *selectionStorage) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	s.sizes++
	return s.Storer.EncodedObjectSize(h)
}

// BenchmarkFetchObjectSelection excludes pack encoding and network I/O.
// Half the distinct blobs are below the 1 KiB limit and half above it.
func BenchmarkFetchObjectSelection(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("blobs=%d", count), func(b *testing.B) {
			st := memory.NewStorage()
			put := func(kind plumbing.ObjectType, data string) plumbing.Hash {
				o := st.NewEncodedObject()
				o.SetType(kind)
				w, err := o.Writer()
				require.NoError(b, err)
				_, err = w.Write([]byte(data))
				require.NoError(b, err)
				require.NoError(b, w.Close())
				h, err := st.SetEncodedObject(o)
				require.NoError(b, err)
				return h
			}
			entries := make([]object.TreeEntry, 0, count)
			for i := range count {
				content := fmt.Sprintf("%08d", i) + strings.Repeat("x", 32+2016*(i%2))
				h := put(plumbing.BlobObject, content)
				entries = append(entries, object.TreeEntry{Name: fmt.Sprintf("%08d", i), Mode: filemode.Regular, Hash: h})
			}
			o := st.NewEncodedObject()
			require.NoError(b, (&object.Tree{Entries: entries}).Encode(o))
			tree, err := st.SetEncodedObject(o)
			require.NoError(b, err)
			commit := fmt.Sprintf("tree %s\nauthor A <a@b> 1 +0000\ncommitter A <a@b> 1 +0000\n\nselection\n", tree)
			tip := put(plumbing.CommitObject, commit)
			wants := []plumbing.Hash{tip}
			for _, mode := range []string{"baseline", "unfiltered", "blob:none", "blob:limit=1k"} {
				b.Run(mode, func(b *testing.B) {
					filter := packp.Filter(mode)
					if mode == "baseline" || mode == "unfiltered" {
						filter = ""
					}
					limit, err := parseFetchFilter(filter)
					if err != nil {
						b.Fatal(err)
					}
					counted := &selectionStorage{Storer: st}
					b.ReportAllocs()
					for b.Loop() {
						var objs []plumbing.Hash
						if mode == "baseline" {
							objs, err = objectsToUpload(counted, wants, nil)
						} else {
							objs, err = revlist.ObjectsWithOptions(counted, wants, nil, revlist.ObjectsOptions{BlobLimit: limit, IncludeWants: true})
						}
						if err != nil {
							b.Fatal(err)
						}
						if len(objs) < 2 {
							b.Fatal("missing commit or tree")
						}
					}
					b.ReportMetric(float64(counted.reads)/float64(b.N), "object-reads/op")
					b.ReportMetric(float64(counted.sizes)/float64(b.N), "size-reads/op")
				})
			}
		})
	}
}
