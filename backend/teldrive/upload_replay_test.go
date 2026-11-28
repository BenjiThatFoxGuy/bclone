package teldrive

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
)

// A retry of a single-part upload used to skip the existing-parts lookup
// entirely, because the lookup was gated on the file being larger than one
// chunk. The server could then not tell the client that its bytes were already
// on Telegram, so every retry left another copy in the storage channel.
func TestPrepareUploadChecksExistingPartsForSingleChunkFile(t *testing.T) {
	var partLookups int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/uploads/") {
			partLookups++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx := context.Background()
	f := &Fs{
		root:         "",
		name:         "test",
		rootFolderID: "root-folder",
		userId:       1,
		opt:          Options{ChunkSize: 100 * fs.Mebi}, // far larger than the file
		srv:          rest.NewClient(fshttp.NewClient(ctx)).SetRoot(srv.URL),
		pacer:        fs.NewPacer(ctx, pacer.NewDefault()),
	}
	f.dirCache = dircache.New(f.root, f.rootFolderID, f)

	o := &Object{fs: f, remote: "song.flac"}
	src := object.NewStaticObjectInfo("song.flac", time.Now(), 100, true, nil, f)

	info, err := o.prepareUpload(ctx, src)
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, 1, partLookups, "single-part upload must ask the server for existing parts")
	assert.NotNil(t, info.existingChunks, "an empty existing-parts map must still be built")
}

// A genuine overwrite (same name and size, different content) must not be
// mistaken for a replay of the old upload. Modtime is part of the session id
// so the two get different sessions.
func TestPrepareUploadSessionIDIncludesModTime(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/uploads/") {
			paths = append(paths, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx := context.Background()
	f := &Fs{
		root:         "",
		name:         "test",
		rootFolderID: "root-folder",
		userId:       1,
		opt:          Options{ChunkSize: 100 * fs.Mebi},
		srv:          rest.NewClient(fshttp.NewClient(ctx)).SetRoot(srv.URL),
		pacer:        fs.NewPacer(ctx, pacer.NewDefault()),
	}
	f.dirCache = dircache.New(f.root, f.rootFolderID, f)
	o := &Object{fs: f, remote: "song.flac"}

	base := time.Now()
	for _, modTime := range []time.Time{base, base.Add(time.Minute)} {
		src := object.NewStaticObjectInfo("song.flac", modTime, 100, true, nil, f)
		_, err := o.prepareUpload(ctx, src)
		require.NoError(t, err)
	}
	require.Len(t, paths, 2)
	assert.NotEqual(t, paths[0], paths[1], "different modtimes must use different upload sessions")
}
