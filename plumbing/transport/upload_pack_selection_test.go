package transport

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v6/storage"
	"github.com/go-git/go-git/v6/storage/memory"
)

type fetchWalkerStorage struct {
	storage.Storer
	called               bool
	wants, haves, result []plumbing.Hash
}

func (s *fetchWalkerStorage) RevListObjects(wants, haves []plumbing.Hash) ([]plumbing.Hash, error) {
	s.called = true
	s.wants, s.haves = wants, haves
	return s.result, nil
}

func TestUploadPackV2ExplicitTreeClosure(t *testing.T) {
	t.Parallel()
	for _, format := range []formatcfg.ObjectFormat{formatcfg.SHA1, formatcfg.SHA256} {
		t.Run(format.String(), func(t *testing.T) {
			t.Parallel()
			st := memory.NewStorage(memory.WithObjectFormat(format))
			put := func(kind plumbing.ObjectType, data string) plumbing.Hash {
				o := st.NewEncodedObject()
				o.SetType(kind)
				w, err := o.Writer()
				require.NoError(t, err)
				_, err = w.Write([]byte(data))
				require.NoError(t, err)
				require.NoError(t, w.Close())
				h, err := st.SetEncodedObject(o)
				require.NoError(t, err)
				return h
			}
			tree := func(entries []object.TreeEntry) plumbing.Hash {
				o := st.NewEncodedObject()
				require.NoError(t, (&object.Tree{Entries: entries}).Encode(o))
				h, err := st.SetEncodedObject(o)
				require.NoError(t, err)
				return h
			}
			small, large := put(plumbing.BlobObject, "abc"), put(plumbing.BlobObject, "123456789")
			sub := tree([]object.TreeEntry{{Name: "small", Mode: filemode.Regular, Hash: small}})
			root := tree([]object.TreeEntry{{Name: "large", Mode: filemode.Regular, Hash: large}, {Name: "sub", Mode: filemode.Dir, Hash: sub}})
			commit := put(plumbing.CommitObject, fmt.Sprintf("tree %s\nauthor A <a@b> 1 +0000\ncommitter A <a@b> 1 +0000\n\nc\n", root))
			tag := put(plumbing.TagObject, fmt.Sprintf("object %s\ntype tree\ntag t\ntagger A <a@b> 1 +0000\n\nt\n", root))
			nested := put(plumbing.TagObject, fmt.Sprintf("object %s\ntype tag\ntag outer\ntagger A <a@b> 1 +0000\n\nt\n", tag))
			for _, tc := range []struct {
				name, filter string
				blobs        []plumbing.Hash
			}{
				{name: "unfiltered", blobs: []plumbing.Hash{small, large}},
				{name: "blob-none", filter: "blob:none"},
				{name: "blob-limit", filter: "blob:limit=4", blobs: []plumbing.Hash{small}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					for _, want := range []plumbing.Hash{root, nested} {
						for _, shallow := range []bool{false, true} {
							args := []string{"want " + want.String(), "have " + commit.String(), "done"}
							if tc.filter != "" {
								args = append(args, "filter "+tc.filter)
							}
							if shallow {
								args = append(args, "shallow "+commit.String())
							}
							data := serveUploadPackV2Test(t, st, v2Request(t, "fetch", []string{"object-format=" + format.String()}, args))
							r := strings.NewReader(data)
							var out packp.FetchOutput
							require.NoError(t, out.Decode(r))
							dst := memory.NewStorage(memory.WithObjectFormat(format))
							require.NoError(t, packfile.UpdateObjectStorage(dst, sideband.NewDemuxer(sideband.Sideband64k, r)))
							require.Len(t, dst.Trees, 2)
							require.Empty(t, dst.Commits)
							require.Len(t, dst.Blobs, len(tc.blobs))
							for _, h := range tc.blobs {
								require.NoError(t, dst.HasEncodedObject(h))
							}
							if want == nested {
								require.Len(t, dst.Tags, 2)
							}
						}
					}
				})
			}
		})
	}
}

func TestUploadPackV2UnfilteredSpecializedWalker(t *testing.T) {
	t.Parallel()
	st := memory.NewStorage()
	obj := st.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	require.NoError(t, err)
	_, err = w.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	h, err := st.SetEncodedObject(obj)
	require.NoError(t, err)
	for _, present := range []bool{false, true} {
		t.Run(fmt.Sprintf("already-selected-%v", present), func(t *testing.T) {
			t.Parallel()
			backend := &fetchWalkerStorage{Storer: st}
			if present {
				backend.result = []plumbing.Hash{h}
			}
			data := serveUploadPackV2Test(t, backend, v2Request(t, "fetch", nil, []string{"want " + h.String(), "have " + h.String(), "done"}))
			require.True(t, backend.called)
			require.Equal(t, []plumbing.Hash{h}, backend.wants)
			require.Equal(t, []plumbing.Hash{h}, backend.haves)
			r := strings.NewReader(data)
			var out packp.FetchOutput
			require.NoError(t, out.Decode(r))
			dst := memory.NewStorage()
			require.NoError(t, packfile.UpdateObjectStorage(dst, sideband.NewDemuxer(sideband.Sideband64k, r)))
			require.NoError(t, dst.HasEncodedObject(h))
			require.Len(t, dst.Objects, 1)
		})
	}
}

func TestUploadPackV2SpecializedCommitWalk(t *testing.T) {
	t.Parallel()
	st := memory.NewStorage()
	obj := st.NewEncodedObject()
	obj.SetType(plumbing.CommitObject)
	w, err := obj.Writer()
	require.NoError(t, err)
	// The specialized walker owns traversal; a commit-only result must not
	// trigger a second generic walk of its tree or history.
	_, err = fmt.Fprintf(w, "tree %s\nauthor A <a@b> 1 +0000\ncommitter A <a@b> 1 +0000\n\nc\n", strings.Repeat("1", 40))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	h, err := st.SetEncodedObject(obj)
	require.NoError(t, err)
	backend := &fetchWalkerStorage{Storer: st, result: []plumbing.Hash{h}}
	serveUploadPackV2Test(t, backend, v2Request(t, "fetch", nil, []string{"want " + h.String(), "done"}))
	require.True(t, backend.called)
	require.Equal(t, []plumbing.Hash{h}, backend.wants)
	require.Empty(t, backend.haves)
}
