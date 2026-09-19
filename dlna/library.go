// Package dlna implements enough of UPnP AV / DLNA to let a Roku's Media
// Player browse and play files from this machine.
//
// Roku removed the ECP route for handing a device a URL, but Roku Media
// Player still speaks DLNA. A DLNA MediaServer is three things: an SSDP
// presence on the network, an XML description of itself, and a
// ContentDirectory service that answers Browse over SOAP. All of it is
// plain HTTP and XML, so none of this needs a dependency.
package dlna

import (
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Class is the UPnP class of an object, which is how a renderer decides
// whether something is a folder, a film, a song or a photo.
type Class string

const (
	ClassFolder Class = "object.container.storageFolder"
	ClassVideo  Class = "object.item.videoItem"
	ClassAudio  Class = "object.item.audioItem.musicTrack"
	ClassImage  Class = "object.item.imageItem.photo"
)

// Object is one entry in the content tree: either a container or an item.
type Object struct {
	ID       string
	ParentID string
	Title    string
	Class    Class
	Path     string // on-disk path; empty for the synthetic root
	Size     int64
	MIME     string

	Children []string // object IDs, for containers
}

// IsContainer reports whether the object holds other objects.
func (o *Object) IsContainer() bool { return o.Class == ClassFolder }

// Library is the content tree served to renderers.
//
// Object IDs are assigned at scan time and are stable for the life of the
// server, which is all UPnP requires: "0" is always the root.
type Library struct {
	Root string

	mu      sync.RWMutex
	objects map[string]*Object
	nextID  int
}

// NewLibrary walks root and builds the tree. Files it does not recognise as
// media are skipped, so the TV is not offered text files it cannot open.
func NewLibrary(root string) (*Library, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", abs)
	}

	l := &Library{
		Root:    abs,
		objects: map[string]*Object{},
		nextID:  1,
	}
	// "0" is the reserved root ID in every UPnP ContentDirectory.
	l.objects["0"] = &Object{
		ID:    "0",
		Title: filepath.Base(abs),
		Class: ClassFolder,
		Path:  abs,
	}
	if err := l.scan("0", abs); err != nil {
		return nil, err
	}
	return l, nil
}

// scan populates one directory, recursing into subdirectories. Unreadable
// entries are skipped rather than failing the whole scan: one bad
// permission should not cost you the library.
func (l *Library) scan(parentID, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		// Ordering only, so the cheap link-level check is fine here; the
		// authoritative directory test happens below via os.Stat.
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir() // folders first, as renderers expect
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})

	parent := l.objects[parentID]
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(dir, e.Name())

		// Stat the target rather than the entry: os.ReadDir reports on the
		// link itself, so a symlinked file would be advertised with the
		// length of the link instead of the media, and a symlinked folder
		// would not be recognised as a folder at all. Symlinked media
		// libraries are common enough to be worth following.
		info, err := os.Stat(path)
		if err != nil {
			continue // broken link or unreadable
		}

		if info.IsDir() {
			id := l.newID()
			l.objects[id] = &Object{
				ID: id, ParentID: parentID, Title: e.Name(),
				Class: ClassFolder, Path: path,
			}
			parent.Children = append(parent.Children, id)
			if err := l.scan(id, path); err != nil {
				continue // unreadable subdirectory; keep the rest
			}
			continue
		}

		class, mimeType := classify(e.Name())
		if class == "" {
			continue // not media
		}
		id := l.newID()
		l.objects[id] = &Object{
			ID: id, ParentID: parentID,
			Title: strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())),
			Class: class, Path: path, Size: info.Size(), MIME: mimeType,
		}
		parent.Children = append(parent.Children, id)
	}
	return nil
}

func (l *Library) newID() string {
	id := strconv.Itoa(l.nextID)
	l.nextID++
	return id
}

// Get returns an object by ID.
func (l *Library) Get(id string) (*Object, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	o, ok := l.objects[id]
	return o, ok
}

// Children returns one page of a container's children, along with the total
// count. UPnP Browse is a paging API: count 0 means "all from here".
func (l *Library) Children(id string, start, count int) ([]*Object, int) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	parent, ok := l.objects[id]
	if !ok {
		return nil, 0
	}
	total := len(parent.Children)
	if start > total {
		start = total
	}
	end := total
	if count > 0 && start+count < total {
		end = start + count
	}
	var out []*Object
	for _, cid := range parent.Children[start:end] {
		if o, ok := l.objects[cid]; ok {
			out = append(out, o)
		}
	}
	return out, total
}

// Count reports how many objects the library holds, excluding the root.
func (l *Library) Count() (folders, files int) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for id, o := range l.objects {
		if id == "0" {
			continue
		}
		if o.IsContainer() {
			folders++
		} else {
			files++
		}
	}
	return folders, files
}

// classify maps a filename to a UPnP class and MIME type, and reports an
// empty class for anything that is not playable media.
//
// The explicit table exists because mime.TypeByExtension is inconsistent
// across systems for exactly the container formats that matter here, and a
// renderer that gets the wrong protocolInfo will refuse to play the file.
// The clearest case: this system's table reports ".webm" as "audio/webm",
// which would file every webm video under music and offer it to the TV with
// no video stream declared.
func classify(name string) (Class, string) {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mp4", ".m4v":
		return ClassVideo, "video/mp4"
	case ".mkv":
		return ClassVideo, "video/x-matroska"
	case ".mov":
		return ClassVideo, "video/quicktime"
	case ".avi":
		return ClassVideo, "video/x-msvideo"
	case ".webm":
		return ClassVideo, "video/webm"
	case ".ts":
		return ClassVideo, "video/mp2t"
	case ".mpg", ".mpeg":
		return ClassVideo, "video/mpeg"
	case ".mp3":
		return ClassAudio, "audio/mpeg"
	case ".m4a", ".aac":
		return ClassAudio, "audio/mp4"
	case ".flac":
		return ClassAudio, "audio/flac"
	case ".wav":
		return ClassAudio, "audio/wav"
	case ".ogg", ".oga":
		return ClassAudio, "audio/ogg"
	case ".opus":
		// Opus normally travels in an Ogg container, and that is the type
		// renderers match on.
		return ClassAudio, "audio/ogg"
	case ".jpg", ".jpeg":
		return ClassImage, "image/jpeg"
	case ".png":
		return ClassImage, "image/png"
	case ".gif":
		return ClassImage, "image/gif"
	case ".webp":
		return ClassImage, "image/webp"
	}
	// Fall back to the system table for anything else that is clearly media.
	if t := mime.TypeByExtension(ext); t != "" {
		switch {
		case strings.HasPrefix(t, "video/"):
			return ClassVideo, t
		case strings.HasPrefix(t, "audio/"):
			return ClassAudio, t
		case strings.HasPrefix(t, "image/"):
			return ClassImage, t
		}
	}
	return "", ""
}
