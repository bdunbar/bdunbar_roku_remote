package dlna

import (
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// buildLibrary creates a small tree on disk and returns a Library over it.
func buildLibrary(t *testing.T) *Library {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "clip.mp4"), 2048)
	mustWrite(t, filepath.Join(root, "song.mp3"), 512)
	mustWrite(t, filepath.Join(root, "notes.txt"), 10) // must be ignored
	mustWrite(t, filepath.Join(root, ".hidden.mp4"), 10)

	sub := filepath.Join(root, "Holiday")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(sub, "beach.jpg"), 128)

	lib, err := NewLibrary(root)
	if err != nil {
		t.Fatal(err)
	}
	return lib
}

func mustWrite(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLibrarySkipsNonMedia(t *testing.T) {
	lib := buildLibrary(t)
	folders, files := lib.Count()
	if folders != 1 {
		t.Errorf("folders = %d, want 1", folders)
	}
	// clip.mp4, song.mp3, beach.jpg — not notes.txt and not the dotfile.
	if files != 3 {
		t.Errorf("files = %d, want 3 (txt and dotfiles must be skipped)", files)
	}
}

func TestLibraryFoldersSortFirst(t *testing.T) {
	lib := buildLibrary(t)
	children, total := lib.Children("0", 0, 0)
	if total != 3 {
		t.Fatalf("root has %d children, want 3", total)
	}
	if !children[0].IsContainer() {
		t.Errorf("first child is %q, want the folder first", children[0].Title)
	}
}

func TestLibraryPaging(t *testing.T) {
	lib := buildLibrary(t)
	page, total := lib.Children("0", 1, 1)
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if len(page) != 1 {
		t.Fatalf("page has %d entries, want 1", len(page))
	}
	// A start index past the end must not panic and must return nothing.
	if page, _ := lib.Children("0", 99, 10); len(page) != 0 {
		t.Errorf("out-of-range start returned %d entries", len(page))
	}
}

func TestLibraryFollowsSymlinks(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(t.TempDir(), "real.mp4")
	mustWrite(t, real, 4096)
	if err := os.Symlink(real, filepath.Join(root, "link.mp4")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	lib, err := NewLibrary(root)
	if err != nil {
		t.Fatal(err)
	}
	children, _ := lib.Children("0", 0, 0)
	if len(children) != 1 {
		t.Fatalf("got %d children, want 1", len(children))
	}
	// The whole point: the target's size, not the link's.
	if children[0].Size != 4096 {
		t.Errorf("size = %d, want 4096 (stat must follow the link)", children[0].Size)
	}
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name  string
		class Class
		mime  string
	}{
		{"a.mp4", ClassVideo, "video/mp4"},
		{"a.MKV", ClassVideo, "video/x-matroska"},
		{"a.mp3", ClassAudio, "audio/mpeg"},
		{"a.jpg", ClassImage, "image/jpeg"},
		{"a.opus", ClassAudio, "audio/ogg"},
		// The system MIME table calls this audio/webm; a webm is video here.
		{"a.webm", ClassVideo, "video/webm"},
		{"a.txt", "", ""},
		{"noext", "", ""},
	} {
		class, mime := classify(tc.name)
		if class != tc.class || mime != tc.mime {
			t.Errorf("classify(%q) = %q/%q, want %q/%q", tc.name, class, mime, tc.class, tc.mime)
		}
	}
}

func TestContentFeaturesSeekability(t *testing.T) {
	// Video must advertise byte-range seeking; a still image must not.
	if got := contentFeatures(&Object{Class: ClassVideo}); !strings.Contains(got, "DLNA.ORG_OP=01") {
		t.Errorf("video features = %q, want OP=01", got)
	}
	if got := contentFeatures(&Object{Class: ClassImage}); !strings.Contains(got, "DLNA.ORG_OP=00") {
		t.Errorf("image features = %q, want OP=00", got)
	}
}

func TestDIDLEscaping(t *testing.T) {
	o := &Object{ID: "1", ParentID: "0", Title: `Tom & Jerry <best>`, Class: ClassVideo, MIME: "video/mp4", Path: "/x/a.mp4"}
	got := didl([]*Object{o}, "http://host:1")
	if strings.Contains(got, "<best>") {
		t.Error("title was not escaped into the document")
	}
	if !strings.Contains(got, "&amp;") {
		t.Error("ampersand was not escaped")
	}
	if !strings.Contains(got, "/media/1.mp4") {
		t.Errorf("resource URL missing or wrong: %s", got)
	}
}

func TestMakeUDNIsStable(t *testing.T) {
	a := makeUDN("host", "/srv/media")
	if b := makeUDN("host", "/srv/media"); a != b {
		t.Error("UDN changed between calls for identical inputs")
	}
	if c := makeUDN("host", "/other"); a == c {
		t.Error("different directories produced the same UDN")
	}
	if !strings.HasPrefix(a, "uuid:") {
		t.Errorf("UDN %q must carry the uuid: prefix", a)
	}
}

// newTestServer wires a Server around a temp library without binding SSDP.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	lib := buildLibrary(t)
	return &Server{Library: lib, FriendlyName: "test", udn: makeUDN("test", lib.Root)}
}

func browse(t *testing.T, s *Server, objectID, flag string) string {
	t.Helper()
	body := `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
		`<u:Browse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">` +
		`<ObjectID>` + objectID + `</ObjectID><BrowseFlag>` + flag + `</BrowseFlag>` +
		`<Filter>*</Filter><StartingIndex>0</StartingIndex><RequestedCount>0</RequestedCount>` +
		`<SortCriteria></SortCriteria></u:Browse></s:Body></s:Envelope>`
	req := httptest.NewRequest(http.MethodPost, "/control/cds", strings.NewReader(body))
	req.Host = "host:1"
	w := httptest.NewRecorder()
	s.handleControl(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Browse returned %d: %s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

var resultRE = regexp.MustCompile(`(?s)<Result>(.*?)</Result>`)

func TestBrowseDirectChildren(t *testing.T) {
	s := newTestServer(t)
	out := browse(t, s, "0", "BrowseDirectChildren")
	if !strings.Contains(out, "<TotalMatches>3</TotalMatches>") {
		t.Errorf("wrong TotalMatches in %s", out)
	}
	m := resultRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatal("no Result element")
	}
	// The DIDL document travels escaped inside the SOAP body.
	didlDoc := html.UnescapeString(m[1])
	if !strings.Contains(didlDoc, "<DIDL-Lite") {
		t.Errorf("Result did not contain a DIDL-Lite document: %s", didlDoc)
	}
	if !strings.Contains(didlDoc, "object.container.storageFolder") {
		t.Errorf("folder missing from listing: %s", didlDoc)
	}
}

func TestBrowseMetadata(t *testing.T) {
	s := newTestServer(t)
	out := browse(t, s, "0", "BrowseMetadata")
	if !strings.Contains(out, "<NumberReturned>1</NumberReturned>") {
		t.Errorf("BrowseMetadata should return exactly one object: %s", out)
	}
}

func TestBrowseUnknownObjectFaults(t *testing.T) {
	s := newTestServer(t)
	body := `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
		`<u:Browse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">` +
		`<ObjectID>does-not-exist</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag>` +
		`<Filter>*</Filter><StartingIndex>0</StartingIndex><RequestedCount>0</RequestedCount>` +
		`<SortCriteria></SortCriteria></u:Browse></s:Body></s:Envelope>`
	req := httptest.NewRequest(http.MethodPost, "/control/cds", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleControl(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 for a UPnP fault", w.Code)
	}
	if !strings.Contains(w.Body.String(), "<errorCode>701</errorCode>") {
		t.Errorf("want UPnP error 701 (no such object), got %s", w.Body.String())
	}
}

func TestHandleMediaServesFileWithDLNAHeaders(t *testing.T) {
	s := newTestServer(t)
	children, _ := s.Library.Children("0", 0, 0)
	var item *Object
	for _, c := range children {
		if !c.IsContainer() {
			item = c
			break
		}
	}
	if item == nil {
		t.Fatal("no media item at the root")
	}

	req := httptest.NewRequest(http.MethodGet, "/media/"+item.ID+".mp4", nil)
	w := httptest.NewRecorder()
	s.handleMedia(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Header().Get("contentFeatures.dlna.org"); got == "" {
		t.Error("missing contentFeatures.dlna.org header")
	}
	if got := w.Header().Get("transferMode.dlna.org"); got != "Streaming" {
		t.Errorf("transferMode = %q, want Streaming", got)
	}
	if w.Body.Len() != int(item.Size) {
		t.Errorf("served %d bytes, want %d", w.Body.Len(), item.Size)
	}
}

func TestHandleMediaRangeRequest(t *testing.T) {
	s := newTestServer(t)
	children, _ := s.Library.Children("0", 0, 0)
	var item *Object
	for _, c := range children {
		if !c.IsContainer() {
			item = c
			break
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/media/"+item.ID+".mp4", nil)
	req.Header.Set("Range", "bytes=10-19")
	w := httptest.NewRecorder()
	s.handleMedia(w, req)
	// Seeking depends on this: without 206 the TV cannot scrub.
	if w.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", w.Code)
	}
	if w.Body.Len() != 10 {
		t.Errorf("got %d bytes, want 10", w.Body.Len())
	}
}

func TestHandleMediaRejectsUnknownID(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/media/9999.mp4", nil)
	w := httptest.NewRecorder()
	s.handleMedia(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestSSDPHeaderValueIsCaseInsensitive(t *testing.T) {
	msg := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nst: ssdp:all\r\nMX: 3\r\n\r\n"
	if got := headerValue(msg, "ST"); got != "ssdp:all" {
		t.Errorf("headerValue(ST) = %q", got)
	}
	if got := headerValue(msg, "mx"); got != "3" {
		t.Errorf("headerValue(mx) = %q", got)
	}
	if got := headerValue(msg, "absent"); got != "" {
		t.Errorf("missing header returned %q", got)
	}
}

func TestSSDPTargetsAndUSN(t *testing.T) {
	s := &ssdpResponder{udn: "uuid:abc"}
	targets := s.targets()
	if len(targets) != 5 {
		t.Errorf("advertising %d targets, want 5", len(targets))
	}
	// The USN for the device's own UDN must not be doubled up.
	if got := s.usnFor("uuid:abc"); got != "uuid:abc" {
		t.Errorf("usnFor(udn) = %q, want the bare UDN", got)
	}
	if got := s.usnFor(deviceType); got != "uuid:abc::"+deviceType {
		t.Errorf("usnFor(deviceType) = %q", got)
	}
}
