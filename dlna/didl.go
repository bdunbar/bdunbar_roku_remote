package dlna

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// DIDL-Lite is the metadata format a ContentDirectory returns. It is XML
// that travels *inside* an XML element, escaped — which is as awkward as it
// sounds, and is why the result is built as a string and then escaped by
// the SOAP encoder rather than nested as structs.

// dlnaFlags is the DLNA.ORG_FLAGS value advertising streaming media that
// supports seeking. Renderers use it to decide whether to offer a scrub bar.
const dlnaFlags = "01700000000000000000000000000000"

// contentFeatures builds the DLNA.ORG_* protocol annotation for an item.
// DLNA.ORG_OP=01 declares byte-range seeking, which is what makes fast
// forward work; images are not seekable so they get 00.
func contentFeatures(o *Object) string {
	op := "01"
	if o.Class == ClassImage {
		op = "00"
	}
	return fmt.Sprintf("DLNA.ORG_OP=%s;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=%s", op, dlnaFlags)
}

// protocolInfo describes how an item can be fetched. Renderers match on
// this before they will even try a resource.
func protocolInfo(o *Object) string {
	return fmt.Sprintf("http-get:*:%s:%s", o.MIME, contentFeatures(o))
}

// didl renders a set of objects as a DIDL-Lite document.
//
// baseURL is where this server can be reached by the renderer, which is not
// necessarily where it is reached by anyone else — the address has to be
// the one on the TV's own network.
func didl(objects []*Object, baseURL string) string {
	var b strings.Builder
	b.WriteString(`<DIDL-Lite ` +
		`xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" ` +
		`xmlns:dc="http://purl.org/dc/elements/1.1/" ` +
		`xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/" ` +
		`xmlns:dlna="urn:schemas-dlna-org:metadata-1-0/">`)

	for _, o := range objects {
		if o.IsContainer() {
			fmt.Fprintf(&b,
				`<container id="%s" parentID="%s" restricted="1" childCount="%d">`+
					`<dc:title>%s</dc:title>`+
					`<upnp:class>%s</upnp:class>`+
					`</container>`,
				esc(o.ID), esc(o.ParentID), len(o.Children), esc(o.Title), o.Class)
			continue
		}
		fmt.Fprintf(&b,
			`<item id="%s" parentID="%s" restricted="1">`+
				`<dc:title>%s</dc:title>`+
				`<upnp:class>%s</upnp:class>`+
				`<res protocolInfo="%s" size="%d">%s</res>`+
				`</item>`,
			esc(o.ID), esc(o.ParentID), esc(o.Title), o.Class,
			esc(protocolInfo(o)), o.Size, esc(resourceURL(baseURL, o)))
	}
	b.WriteString(`</DIDL-Lite>`)
	return b.String()
}

// resourceURL is where a renderer fetches an item's bytes. The file
// extension is kept on the end because some renderers sniff it in
// preference to the Content-Type header.
func resourceURL(baseURL string, o *Object) string {
	return baseURL + "/media/" + o.ID + extOf(o.Path)
}

func extOf(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i:]
	}
	return ""
}

// esc XML-escapes a value for inclusion in the document.
func esc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}
