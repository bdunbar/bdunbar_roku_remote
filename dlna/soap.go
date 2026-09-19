package dlna

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// soapEnvelope is the outer shell of every UPnP control message.
type soapEnvelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Body    soapBody `xml:"Body"`
}

type soapBody struct {
	Browse                  *browseRequest `xml:"Browse"`
	GetSearchCapabilities   *struct{}      `xml:"GetSearchCapabilities"`
	GetSortCapabilities     *struct{}      `xml:"GetSortCapabilities"`
	GetSystemUpdateID       *struct{}      `xml:"GetSystemUpdateID"`
	GetProtocolInfo         *struct{}      `xml:"GetProtocolInfo"`
	GetCurrentConnectionIDs *struct{}      `xml:"GetCurrentConnectionIDs"`
}

// browseRequest is the one action that matters: it is how a renderer walks
// the content tree.
type browseRequest struct {
	ObjectID       string `xml:"ObjectID"`
	BrowseFlag     string `xml:"BrowseFlag"`
	Filter         string `xml:"Filter"`
	StartingIndex  int    `xml:"StartingIndex"`
	RequestedCount int    `xml:"RequestedCount"`
	SortCriteria   string `xml:"SortCriteria"`
}

// handleControl answers SOAP calls on the ContentDirectory and
// ConnectionManager services.
func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "SOAP control accepts POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		soapFault(w, 402, "Invalid Args")
		return
	}

	var env soapEnvelope
	if err := xml.Unmarshal(body, &env); err != nil {
		soapFault(w, 401, "Invalid Action")
		return
	}

	switch {
	case env.Body.Browse != nil:
		s.handleBrowse(w, r, env.Body.Browse)
	case env.Body.GetSearchCapabilities != nil:
		// We do not implement Search, and saying so plainly is correct:
		// renderers fall back to Browse rather than failing.
		soapReply(w, "GetSearchCapabilities", "ContentDirectory",
			map[string]string{"SearchCaps": ""})
	case env.Body.GetSortCapabilities != nil:
		soapReply(w, "GetSortCapabilities", "ContentDirectory",
			map[string]string{"SortCaps": ""})
	case env.Body.GetSystemUpdateID != nil:
		soapReply(w, "GetSystemUpdateID", "ContentDirectory",
			map[string]string{"Id": "1"})
	case env.Body.GetProtocolInfo != nil:
		soapReply(w, "GetProtocolInfo", "ConnectionManager", map[string]string{
			"Source": "http-get:*:video/mp4:*,http-get:*:video/x-matroska:*," +
				"http-get:*:audio/mpeg:*,http-get:*:image/jpeg:*,http-get:*:image/png:*",
			"Sink": "",
		})
	case env.Body.GetCurrentConnectionIDs != nil:
		soapReply(w, "GetCurrentConnectionIDs", "ConnectionManager",
			map[string]string{"ConnectionIDs": ""})
	default:
		soapFault(w, 401, "Invalid Action")
	}
}

// handleBrowse answers the Browse action, which comes in two flavours:
// metadata about one object, or the list of a container's children.
func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request, req *browseRequest) {
	obj, ok := s.Library.Get(req.ObjectID)
	if !ok {
		soapFault(w, 701, "No such object")
		return
	}
	base := s.baseURLFor(r)

	var (
		result   string
		returned int
		total    int
	)
	switch req.BrowseFlag {
	case "BrowseMetadata":
		result = didl([]*Object{obj}, base)
		returned, total = 1, 1
	default: // BrowseDirectChildren
		children, n := s.Library.Children(req.ObjectID, req.StartingIndex, req.RequestedCount)
		result = didl(children, base)
		returned, total = len(children), n
	}

	soapReply(w, "Browse", "ContentDirectory", map[string]string{
		"Result":         result,
		"NumberReturned": strconv.Itoa(returned),
		"TotalMatches":   strconv.Itoa(total),
		"UpdateID":       "1",
	})
}

// soapReply writes a successful SOAP response. Argument order is not
// significant to renderers, but the namespace and the "Response" suffix are.
func soapReply(w http.ResponseWriter, action, service string, args map[string]string) {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" ` +
		`s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&b, `<u:%sResponse xmlns:u="urn:schemas-upnp-org:service:%s:1">`,
		action, service)
	for k, v := range args {
		fmt.Fprintf(&b, "<%s>%s</%s>", k, esc(v), k)
	}
	fmt.Fprintf(&b, `</u:%sResponse>`, action)
	b.WriteString(`</s:Body></s:Envelope>`)

	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.Header().Set("EXT", "")
	io.WriteString(w, b.String())
}

// soapFault reports a UPnP error. The HTTP status must be 500 even though
// the useful detail is in the body.
func soapFault(w http.ResponseWriter, code int, desc string) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprintf(w, xml.Header+
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault>`+
		`<faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail>`+
		`<UPnPError xmlns="urn:schemas-upnp-org:control-1-0">`+
		`<errorCode>%d</errorCode><errorDescription>%s</errorDescription>`+
		`</UPnPError></detail></s:Fault></s:Body></s:Envelope>`, code, esc(desc))
}
