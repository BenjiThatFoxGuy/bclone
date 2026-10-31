package gotohp

import (
	"bytes"
	"context"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/gotohp/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/vfs/vfscommon"
)

func testAuthString() string {
	return "androidId=abc123&app=com.google.android.apps.photos&client_sig=sig&Email=test@example.com&Token=tok&lang=en_US&service=oauth2:https://www.googleapis.com/auth/photos.native"
}

// newTestFs builds a real *Fs (NewFs does no network I/O) with deferred
// uploads active, simulating what NewFs auto-detects when running under
// mount/serve (--vfs-cache-mode writes or full) — see NewFs's use of
// vfscommon.Opt. Used by tests exercising the phantom-cache mechanism
// itself; production toggling of deferUploads is covered separately by
// TestDeferUploadsDefaultIsFalse / TestDeferUploadsAutoDetectedFromVFSCacheMode.
func newTestFs(t *testing.T, root string) *Fs {
	t.Helper()
	prevMode := vfscommon.Opt.CacheMode
	prevWriteBack := vfscommon.Opt.WriteBack
	vfscommon.Opt.CacheMode = vfscommon.CacheModeWrites
	vfscommon.Opt.WriteBack = fs.Duration(5 * time.Minute)
	t.Cleanup(func() {
		vfscommon.Opt.CacheMode = prevMode
		vfscommon.Opt.WriteBack = prevWriteBack
	})

	m := configmap.Simple{"auth": testAuthString()}
	f, err := NewFs(context.Background(), "test", root, m)
	if err != nil {
		t.Fatalf("NewFs: %v", err)
	}
	gf, ok := f.(*Fs)
	if !ok {
		t.Fatalf("NewFs returned %T, want *Fs", f)
	}
	if !gf.deferUploads {
		t.Fatal("expected deferUploads to be auto-detected as true with vfscommon.Opt.CacheMode = writes")
	}
	t.Cleanup(func() { _ = gf.Shutdown(context.Background()) })
	return gf
}

// newSyncTestFs builds a real *Fs with deferred uploads left off, matching
// a plain copy/sync/move process where vfscommon.Opt.CacheMode is never
// touched (stays at its Go zero value, CacheModeOff).
func newSyncTestFs(t *testing.T, root string) *Fs {
	t.Helper()
	m := configmap.Simple{"auth": testAuthString()}
	f, err := NewFs(context.Background(), "test", root, m)
	if err != nil {
		t.Fatalf("NewFs: %v", err)
	}
	gf, ok := f.(*Fs)
	if !ok {
		t.Fatalf("NewFs returned %T, want *Fs", f)
	}
	t.Cleanup(func() { _ = gf.Shutdown(context.Background()) })
	return gf
}

func TestResolvePathLoose(t *testing.T) {
	f := newTestFs(t, "")
	rp := f.resolvePath("photo.jpg")
	if rp.mode != albumNone {
		t.Errorf("mode = %v, want albumNone", rp.mode)
	}
	if rp.leaf != "photo.jpg" {
		t.Errorf("leaf = %q, want photo.jpg", rp.leaf)
	}
}

func TestResolvePathNewAlbum(t *testing.T) {
	f := newTestFs(t, "")
	rp := f.resolvePath("NewAlbum/Vacation/photo.jpg")
	if rp.mode != albumByName {
		t.Errorf("mode = %v, want albumByName", rp.mode)
	}
	if rp.albumRef != "Vacation" {
		t.Errorf("albumRef = %q, want Vacation", rp.albumRef)
	}
	if rp.leaf != "photo.jpg" {
		t.Errorf("leaf = %q, want photo.jpg", rp.leaf)
	}
}

func TestResolvePathExistingAlbum(t *testing.T) {
	f := newTestFs(t, "")
	rp := f.resolvePath("ExistingAlbum/AF1QipSomeID/nested/photo.jpg")
	if rp.mode != albumExisting {
		t.Errorf("mode = %v, want albumExisting", rp.mode)
	}
	if rp.albumRef != "AF1QipSomeID" {
		t.Errorf("albumRef = %q, want AF1QipSomeID", rp.albumRef)
	}
	if rp.leaf != "nested/photo.jpg" {
		t.Errorf("leaf = %q, want nested/photo.jpg", rp.leaf)
	}
}

func TestResolvePathRootPrefix(t *testing.T) {
	// Fs rooted at "NewAlbum/Vacation": Put is called with just "photo.jpg"
	// relative to that root, but routing must still see the full combined
	// path.
	f := newTestFs(t, "NewAlbum/Vacation")
	rp := f.resolvePath("photo.jpg")
	if rp.mode != albumByName {
		t.Errorf("mode = %v, want albumByName", rp.mode)
	}
	if rp.albumRef != "Vacation" {
		t.Errorf("albumRef = %q, want Vacation", rp.albumRef)
	}
	if rp.leaf != "photo.jpg" {
		t.Errorf("leaf = %q, want photo.jpg", rp.leaf)
	}
}

func TestResolvePathConfigAlbumOverridesRouting(t *testing.T) {
	f := newTestFs(t, "")
	f.opt.Album = "PinnedAlbum"
	rp := f.resolvePath("NewAlbum/Ignored/photo.jpg")
	if rp.mode != albumByName {
		t.Errorf("mode = %v, want albumByName", rp.mode)
	}
	if rp.albumRef != "PinnedAlbum" {
		t.Errorf("albumRef = %q, want PinnedAlbum (config album should win over path routing)", rp.albumRef)
	}
	if rp.leaf != "NewAlbum/Ignored/photo.jpg" {
		t.Errorf("leaf = %q, want the raw remote unparsed", rp.leaf)
	}
}

func TestPhantomCacheLifecycle(t *testing.T) {
	f := newTestFs(t, "")

	tmp, err := os.CreateTemp(f.phantomDir, "test-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := tmp.WriteString("hello"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = tmp.Close()

	entry := &phantomEntry{
		size:      5,
		modTime:   time.Now(),
		sha1:      []byte{1, 2, 3},
		localPath: tmp.Name(),
		createdAt: time.Now(),
	}
	f.setPhantom("photo.jpg", entry)

	got, ok := f.getPhantom("photo.jpg")
	if !ok {
		t.Fatal("expected phantom entry to be present")
	}
	if got.size != 5 {
		t.Errorf("size = %d, want 5", got.size)
	}

	// NewObject should find it.
	obj, err := f.NewObject(context.Background(), "photo.jpg")
	if err != nil {
		t.Fatalf("NewObject: %v", err)
	}
	if obj.Size() != 5 {
		t.Errorf("obj.Size() = %d, want 5", obj.Size())
	}

	// Open should serve the real spooled bytes back.
	rc, err := obj.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(data, []byte("hello")) {
		t.Errorf("Open content = %q, want %q", data, "hello")
	}

	// removePhantom should delete both the map entry and the spooled file.
	f.removePhantom("photo.jpg")
	if _, ok := f.getPhantom("photo.jpg"); ok {
		t.Error("expected phantom entry to be gone after removePhantom")
	}
	if _, err := os.Stat(tmp.Name()); !os.IsNotExist(err) {
		t.Errorf("expected spooled file to be removed, stat err = %v", err)
	}

	// NewObject should now report not-found.
	if _, err := f.NewObject(context.Background(), "photo.jpg"); err == nil {
		t.Error("expected error for removed object")
	}
}

func TestSweepPhantomExpiry(t *testing.T) {
	f := newTestFs(t, "")
	f.lingerFor = 0 // anything not "createdAt.Before(cutoff)" false immediately

	tmp, err := os.CreateTemp(f.phantomDir, "test-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	_ = tmp.Close()

	f.setPhantom("old.jpg", &phantomEntry{
		localPath: tmp.Name(),
		createdAt: time.Now().Add(-time.Hour),
	})

	f.sweepPhantom()

	if _, ok := f.getPhantom("old.jpg"); ok {
		t.Error("expected expired phantom entry to be swept")
	}
	if _, err := os.Stat(tmp.Name()); !os.IsNotExist(err) {
		t.Errorf("expected swept spooled file to be removed, stat err = %v", err)
	}
}

func TestListSynthesizesDirsAndObjects(t *testing.T) {
	f := newTestFs(t, "")
	now := time.Now()
	f.setPhantom("NewAlbum/Vacation/a.jpg", &phantomEntry{size: 1, modTime: now, createdAt: now})
	f.setPhantom("loose.jpg", &phantomEntry{size: 2, modTime: now, createdAt: now})

	entries, err := f.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var sawDir, sawObj bool
	for _, e := range entries {
		switch e.Remote() {
		case "NewAlbum":
			_, sawDir = e.(fs.Directory)
		case "loose.jpg":
			_, sawObj = e.(fs.Object)
		}
	}
	if !sawDir {
		t.Error("expected a synthesized NewAlbum directory entry")
	}
	if !sawObj {
		t.Error("expected loose.jpg object entry")
	}

	// A file written into an album that doesn't exist yet shows it in Album/
	f.setPhantom("Album/NewTrip/b.jpg", &phantomEntry{size: 3, modTime: now, createdAt: now})
	entries, err = f.List(context.Background(), "Album")
	if err != nil {
		t.Fatalf("List Album: %v", err)
	}
	sawDir = false
	for _, e := range entries {
		if e.Remote() == "Album/NewTrip" {
			_, sawDir = e.(fs.Directory)
		}
	}
	if !sawDir {
		t.Error("expected a synthesized Album/NewTrip directory entry")
	}

	// An album that already exists is listed once, not again for its new file
	f.albumMu.Lock()
	f.albums["NewTrip"] = "albkey"
	f.albumMu.Unlock()
	entries, err = f.List(context.Background(), "Album")
	if err != nil {
		t.Fatalf("List Album: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.Remote() == "Album/NewTrip" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("Album/NewTrip listed %d times, want 1", n)
	}
}

func TestDeferUploadsDefaultIsFalse(t *testing.T) {
	// A plain copy/sync/move process never touches --vfs-cache-mode, so
	// vfscommon.Opt.CacheMode stays at its Go zero value (CacheModeOff) —
	// exactly what a fresh test binary also has, with no mutation needed.
	gf := newSyncTestFs(t, "")
	if gf.deferUploads {
		t.Error("deferUploads should default to false when --vfs-cache-mode was never set (synchronous mode)")
	}
}

func TestDeferUploadsAutoDetectedFromVFSCacheMode(t *testing.T) {
	// This is exactly what newTestFs sets up; assert it explicitly here too
	// so the auto-detection wiring in NewFs itself has a direct test.
	gf := newTestFs(t, "")
	if !gf.deferUploads {
		t.Error("deferUploads should be true when vfscommon.Opt.CacheMode is not off")
	}
	if gf.lingerFor != 5*time.Minute {
		t.Errorf("lingerFor = %v, want 5m (from vfscommon.Opt.WriteBack)", gf.lingerFor)
	}
}

func TestSyncModeIgnoresPhantomCache(t *testing.T) {
	gf := newSyncTestFs(t, "")

	if gf.deferUploads {
		t.Fatal("deferUploads should be false in sync mode")
	}

	// Even if something ended up in the phantom map, sync mode must not
	// surface it via List/NewObject/Open — those should behave as a plain
	// write-only remote with nothing lingering.
	gf.setPhantom("photo.jpg", &phantomEntry{size: 1, modTime: time.Now(), createdAt: time.Now()})

	entries, err := gf.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, e := range entries {
		if _, isObj := e.(fs.Object); isObj {
			t.Errorf("List returned object %q in sync mode, want only the Album/ExistingAlbum dirs", e.Remote())
		}
	}

	if _, err := gf.NewObject(context.Background(), "photo.jpg"); err != fs.ErrorObjectNotFound {
		t.Errorf("NewObject error = %v, want fs.ErrorObjectNotFound", err)
	}

	obj := &Object{f: gf, remote: "photo.jpg", size: 1, modTime: time.Now()}
	if _, err := obj.Open(context.Background()); err == nil {
		t.Error("expected Open to fail in sync mode")
	}
}

func TestMkdirRmdirAreNoOps(t *testing.T) {
	f := newTestFs(t, "")
	if err := f.Mkdir(context.Background(), "anything"); err != nil {
		t.Errorf("Mkdir: %v", err)
	}
	if err := f.Rmdir(context.Background(), "anything"); err != nil {
		t.Errorf("Rmdir: %v", err)
	}
}

func TestPresentNames(t *testing.T) {
	items := []*api.LibraryItem{
		{MediaKey: "k1", FileName: "shot.png", Timestamp: 1700000000},
		{MediaKey: "k2", FileName: "shot.png", Timestamp: 1700000060},
		{MediaKey: "k3", FileName: "burst.jpg", Timestamp: 1700000100},
		{MediaKey: "k4", FileName: "burst.jpg", Timestamp: 1700000100},
		{MediaKey: "k5", FileName: "unique.jpg", Timestamp: 1},
	}
	names := presentNames(items)
	got := map[string]string{}
	for name, item := range names {
		got[item.MediaKey] = name
	}
	want := map[string]string{
		"k1": "shot-1700000000.png",
		"k2": "shot-1700000060.png",
		"k3": "burst-1700000100-k3.jpg",
		"k4": "burst-1700000100-k4.jpg",
		"k5": "unique.jpg",
	}
	if len(names) != len(items) {
		t.Fatalf("got %d names for %d items: %v", len(names), len(items), got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s listed as %q, want %q", k, got[k], w)
		}
	}
}

func TestDuplicateNamesListedAndFound(t *testing.T) {
	f := newSyncTestFs(t, "")
	ctx := context.Background()
	f.libMu.Lock()
	f.libCache = map[string]*api.LibraryItem{
		"k1": {MediaKey: "k1", FileName: "a.png", Timestamp: 100, CollectionID: "alb"},
		"k2": {MediaKey: "k2", FileName: "a.png", Timestamp: 200, CollectionID: "alb"},
		"k3": {MediaKey: "k3", FileName: "a.png", Timestamp: 300},
	}
	f.libCacheTime = time.Now()
	f.libCacheTTL = time.Hour
	f.libMu.Unlock()
	f.albumMu.Lock()
	f.albums["Trip"] = "alb"
	f.albumByKey["alb"] = "Trip"
	f.albumMu.Unlock()

	entries, err := f.List(ctx, "Album/Trip")
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, e := range entries {
		listed = append(listed, e.Remote())
	}
	sort.Strings(listed)
	if want := []string{"Album/Trip/a-100.png", "Album/Trip/a-200.png"}; strings.Join(listed, ",") != strings.Join(want, ",") {
		t.Fatalf("listed %v, want %v", listed, want)
	}

	o, err := f.NewObject(ctx, "Album/Trip/a-200.png")
	if err != nil {
		t.Fatal(err)
	}
	if o.(*Object).mediaKey != "k2" {
		t.Errorf("found %s, want k2", o.(*Object).mediaKey)
	}

	// Loose at the root there is only one a.png, so it keeps its name
	o, err = f.NewObject(ctx, "a.png")
	if err != nil {
		t.Fatal(err)
	}
	if o.(*Object).mediaKey != "k3" {
		t.Errorf("found %s, want k3", o.(*Object).mediaKey)
	}
}
