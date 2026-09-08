package transport

import (
	"bytes"
	"context"
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
	"github.com/go-git/go-git/v6/storage/memory"
	"github.com/go-git/go-git/v6/utils/ioutil"
)

func TestUploadPackV2BlobFilters(t *testing.T) {
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
			small := put(plumbing.BlobObject, "123")
			boundary := put(plumbing.BlobObject, "1234")
			nine := put(plumbing.BlobObject, "123456789")
			treeObj := st.NewEncodedObject()
			require.NoError(t, (&object.Tree{Entries: []object.TreeEntry{
				{Name: "a", Mode: filemode.Regular, Hash: small},
				{Name: "b", Mode: filemode.Regular, Hash: boundary},
				{Name: "c", Mode: filemode.Regular, Hash: nine},
			}}).Encode(treeObj))
			tree, err := st.SetEncodedObject(treeObj)
			require.NoError(t, err)
			parent := put(plumbing.CommitObject, fmt.Sprintf("tree %s\nauthor A <a@b> 1 +0000\ncommitter A <a@b> 1 +0000\n\nparent\n", tree))
			tip := put(plumbing.CommitObject, fmt.Sprintf("tree %s\nparent %s\nauthor A <a@b> 2 +0000\ncommitter A <a@b> 2 +0000\n\ntip\n", tree, parent))
			tag := put(plumbing.TagObject, fmt.Sprintf("object %s\ntype blob\ntag blob\ntagger A <a@b> 2 +0000\n\ntag\n", boundary))
			require.NoError(t, st.SetReference(plumbing.NewHashReference("refs/tags/blob", tag)))
			fetch := func(t *testing.T, args ...string) (*memory.Storage, packp.FetchOutput) {
				result := serveUploadPackV2Test(t, st, v2Request(t, "fetch", []string{"object-format=" + format.String()}, args))
				r := strings.NewReader(result)
				var out packp.FetchOutput
				require.NoError(t, out.Decode(r))
				require.True(t, out.Packfile)
				dst := memory.NewStorage(memory.WithObjectFormat(format))
				require.NoError(t, packfile.UpdateObjectStorage(dst, sideband.NewDemuxer(sideband.Sideband64k, r)))
				return dst, out
			}
			for _, tc := range []struct {
				filter string
				blobs  int
			}{
				{"blob:none", 0}, {"blob:limit=0", 0}, {"blob:limit=04", 1}, {"blob:limit=0x4", 1}, {"blob:limit=+4", 1}, {"blob:limit=4", 1}, {"blob:limit=5", 2}, {"blob:limit=1k", 3}, {"blob:limit=010", 2}, {"blob:limit=0x10", 3},
			} {
				t.Run(tc.filter, func(t *testing.T) {
					t.Parallel()
					dst, _ := fetch(t, "want "+tip.String(), "filter "+tc.filter, "include-tag", "done")
					require.Len(t, dst.Blobs, tc.blobs)
					require.Len(t, dst.Commits, 2)
					if tc.blobs >= 2 {
						require.Len(t, dst.Tags, 1)
					} else {
						require.Empty(t, dst.Tags)
					}
				})
			}
			dst, _ := fetch(t, "want "+tip.String(), "want "+boundary.String(), "filter blob:none", "done")
			require.Len(t, dst.Blobs, 1)
			require.NoError(t, dst.HasEncodedObject(boundary))
			nested := put(plumbing.TagObject, fmt.Sprintf("object %s\ntype tag\ntag nested\ntagger A <a@b> 2 +0000\n\nnested\n", tag))
			for _, have := range []string{"", "have " + tip.String()} {
				args := []string{"want " + nested.String(), "filter blob:none", "done"}
				if have != "" {
					args = append(args, have)
				}
				if have != "" {
					args = append(args, "shallow "+tip.String())
				}
				tagPack, _ := fetch(t, args...)
				for _, h := range []plumbing.Hash{nested, tag, boundary} {
					require.NoError(t, tagPack.HasEncodedObject(h))
				}
			}
			// Later lazy fetch: the commit is already present, its omitted blob is not.
			for _, filter := range []string{"", "filter blob:none"} {
				args := []string{"want " + boundary.String(), "have " + tip.String(), "done"}
				if filter != "" {
					args = append(args, filter)
				}
				dst, _ = fetch(t, args...)
				require.NoError(t, dst.HasEncodedObject(boundary))
			}
			dst, out := fetch(t, "want "+tip.String(), "deepen 1", "filter blob:none", "done")
			require.Len(t, dst.Commits, 1)
			require.Empty(t, dst.Blobs)
			require.Equal(t, []plumbing.Hash{tip}, out.ShallowInfo.Shallows)
			dst, out = fetch(t, "want "+tip.String(), "have "+tip.String(), "shallow "+tip.String(), "deepen 2", "filter blob:none", "done")
			require.NoError(t, dst.HasEncodedObject(parent))
			require.Empty(t, dst.Blobs)
			require.Contains(t, out.ShallowInfo.Unshallows, tip)
		})
	}
}

func TestUploadPackV2InvalidFilters(t *testing.T) {
	t.Parallel()
	for _, filter := range []string{"tree:0", "combine:blob:none", "blob:limit=", "blob:limit=-1", "blob:limit=08", "blob:limit=0b10", "blob:limit=0o10", "blob:limit=1_0", "blob:limit=18446744073709551616", "blob:limit=18014398509481984k", "blob:limit=1x", "blob:limit=1.5k", ""} {
		t.Run(filter, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := UploadPack(context.Background(), memory.NewStorage(), v2Request(t, "fetch", nil, []string{"filter " + filter, "done"}), ioutil.WriteNopCloser(&out), &UploadPackRequest{GitProtocol: "version=2", StatelessRPC: true})
			require.Error(t, err)
			require.Empty(t, out.Bytes())
		})
	}
	var out bytes.Buffer
	err := UploadPack(context.Background(), memory.NewStorage(), v2Request(t, "fetch", nil, []string{"filter blob:none", "filter blob:limit=1", "done"}), ioutil.WriteNopCloser(&out), &UploadPackRequest{GitProtocol: "version=2", StatelessRPC: true})
	require.Error(t, err)
	require.Empty(t, out.Bytes())
}

func TestFetchFilterUnits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value string
		limit uint64
	}{
		{"010", 8}, {"0x10", 16}, {"0X10", 16}, {"+1", 1}, {"+010", 8}, {" 0x10", 16}, {"0x10k", 16384}, {"0", 0}, {"1k", 1024}, {"1K", 1024}, {"2m", 2 << 20}, {"2M", 2 << 20}, {"3g", 3 << 30}, {"3G", 3 << 30}, {"18446744073709551615", ^uint64(0)},
	} {
		n, err := parseFetchFilter(packp.Filter("blob:limit=" + tc.value))
		require.NoError(t, err)
		require.Equal(t, tc.limit, *n)
	}
}
