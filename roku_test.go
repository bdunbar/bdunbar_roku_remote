package roku

import (
	"strings"
	"testing"
)

func TestNormalizeAddr(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"192.168.1.10", "192.168.1.10:8060"},
		{"192.168.1.10:8060", "192.168.1.10:8060"},
		{"http://192.168.1.10:8060/", "192.168.1.10:8060"},
		{"  roku-tv  ", "roku-tv:8060"},
	} {
		if got := NormalizeAddr(tc.in); got != tc.want {
			t.Errorf("NormalizeAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestYouTubeID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=42s", "dQw4w9WgXcQ"},
		{"https://youtu.be/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"https://youtu.be/dQw4w9WgXcQ?si=abc", "dQw4w9WgXcQ"},
		{"https://www.youtube.com/embed/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"https://www.youtube.com/live/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"https://www.youtube.com/shorts/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
	} {
		got, err := YouTubeID(tc.in)
		if err != nil {
			t.Errorf("YouTubeID(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("YouTubeID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if _, err := YouTubeID("https://example.com/"); err == nil {
		t.Error("expected an error for a URL with no video ID")
	}
}

func TestKindFor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   MediaKind
		format string
	}{
		{"clip.mp4", Video, "mp4"},
		{"Movie.MKV", Video, "mkv"},
		{"photo.JPG", Photo, "jpg"},
		{"song.mp3", Audio, "mp3"},
	} {
		kind, format := KindFor(tc.name)
		if kind != tc.kind || format != tc.format {
			t.Errorf("KindFor(%q) = %v/%q, want %v/%q", tc.name, kind, format, tc.kind, tc.format)
		}
	}
}

func TestSerialFromUSN(t *testing.T) {
	if got := serialFromUSN("uuid:roku:ecp:YS00F6582092"); got != "YS00F6582092" {
		t.Errorf("serialFromUSN = %q", got)
	}
	if got := serialFromUSN(""); got != "" {
		t.Errorf("serialFromUSN(empty) = %q", got)
	}
}

func TestParseSSDP(t *testing.T) {
	resp := strings.Join([]string{
		"HTTP/1.1 200 OK",
		"Cache-Control: max-age=3600",
		"ST: roku:ecp",
		"USN: uuid:roku:ecp:YS00F6582092",
		"LOCATION: http://192.168.1.169:8060/",
		"", "",
	}, "\r\n")
	loc, usn := parseSSDP([]byte(resp))
	if addrFromLocation(loc) != "192.168.1.169:8060" {
		t.Errorf("location = %q", loc)
	}
	if serialFromUSN(usn) != "YS00F6582092" {
		t.Errorf("usn = %q", usn)
	}
}

func TestHostsIn(t *testing.T) {
	ips, err := hostsIn("192.168.1.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 254 {
		t.Fatalf("got %d hosts, want 254", len(ips))
	}
	if ips[0] != "192.168.1.1" || ips[len(ips)-1] != "192.168.1.254" {
		t.Errorf("range is %s..%s", ips[0], ips[len(ips)-1])
	}
	if _, err := hostsIn("nonsense"); err == nil {
		t.Error("expected an error for a bad CIDR")
	}
}

func TestRegistryMatch(t *testing.T) {
	r := &Registry{Devices: []Device{
		{Name: "Living Room TV", Addr: "192.168.1.10:8060", Serial: "AAA"},
		{Name: "Bedroom", Addr: "192.168.1.11:8060", Serial: "BBB"},
	}}
	if got, err := r.Match("bedroom"); err != nil || len(got) != 1 || got[0].Serial != "BBB" {
		t.Errorf("exact name match failed: %v %v", got, err)
	}
	if got, err := r.Match("living"); err != nil || len(got) != 1 || got[0].Serial != "AAA" {
		t.Errorf("substring match failed: %v %v", got, err)
	}
	if got, err := r.Match("all"); err != nil || len(got) != 2 {
		t.Errorf("all match failed: %v %v", got, err)
	}
	if got, err := r.Match("192.168.1.99"); err != nil || got[0].Addr != "192.168.1.99:8060" {
		t.Errorf("bare address passthrough failed: %v %v", got, err)
	}
	if _, err := r.Match("nope"); err == nil {
		t.Error("expected an error for an unknown name")
	}
}

func TestRegistryMergeKeepsAlias(t *testing.T) {
	r := &Registry{Devices: []Device{{Name: "tcl", Addr: "192.168.1.10:8060", Serial: "AAA"}}}
	// Same TV, new DHCP address, reporting its factory name.
	r.Merge([]Device{{Name: "32\" TCL Roku TV", Addr: "192.168.1.55:8060", Serial: "AAA"}})
	if len(r.Devices) != 1 {
		t.Fatalf("merge created a duplicate: %+v", r.Devices)
	}
	if r.Devices[0].Name != "tcl" {
		t.Errorf("alias was lost: %q", r.Devices[0].Name)
	}
	if r.Devices[0].Addr != "192.168.1.55:8060" {
		t.Errorf("address was not refreshed: %q", r.Devices[0].Addr)
	}
}

func TestIsKey(t *testing.T) {
	if got, ok := IsKey("volumeup"); !ok || got != "VolumeUp" {
		t.Errorf("IsKey(volumeup) = %q, %v", got, ok)
	}
	if _, ok := IsKey("Frobnicate"); ok {
		t.Error("IsKey accepted a nonsense key")
	}
}

func TestMatchAppPrefersExactName(t *testing.T) {
	// A real Roku lists both of these, with the longer name first.
	apps := []App{
		{ID: "195316", Name: "YouTube TV"},
		{ID: "837", Name: "YouTube"},
		{ID: "12", Name: "Netflix"},
	}
	got, err := MatchApp(apps, "youtube")
	if err != nil {
		t.Fatalf("MatchApp(youtube): %v", err)
	}
	if got.ID != "837" {
		t.Errorf("matched %s (%s), want YouTube (837) — an exact name must beat a substring", got.Name, got.ID)
	}
	if got, err := MatchApp(apps, "youtube tv"); err != nil || got.ID != "195316" {
		t.Errorf("MatchApp(youtube tv) = %v, %v", got, err)
	}
	if got, err := MatchApp(apps, "837"); err != nil || got.ID != "837" {
		t.Errorf("app ID lookup failed: %v %v", got, err)
	}
}

func TestMatchAppReportsAmbiguity(t *testing.T) {
	apps := []App{
		{ID: "1", Name: "Sports Central"},
		{ID: "2", Name: "Sports Extra"},
	}
	if _, err := MatchApp(apps, "sports"); err == nil {
		t.Error("an ambiguous prefix must be reported, not guessed")
	}
	if _, err := MatchApp(apps, "nothing here"); err == nil {
		t.Error("expected an error for an unmatched query")
	}
}

func TestParseStepForms(t *testing.T) {
	for _, s := range []string{
		"Home", "Select 3", "press Down", "press Down 5",
		"wait 3s", "wait-playing", "wait-playing 45s",
		"select-until-playing", "select-until-playing 30s",
		"launch youtube", "launch 837 contentID=abc",
		"text hello there",
	} {
		if _, err := parseStep(s, nil); err != nil {
			t.Errorf("parseStep(%q): %v", s, err)
		}
	}
}

func TestParseStepRejectsBadInput(t *testing.T) {
	for _, s := range []string{
		"", "Frobnicate", "Select notanumber", "Select 2 3",
		"wait", "wait soon", "launch", "text", "press",
	} {
		if _, err := parseStep(s, nil); err == nil {
			t.Errorf("parseStep(%q) should have failed", s)
		}
	}
}

func TestMacroArgumentSubstitution(t *testing.T) {
	st, err := parseStep("launch youtube contentID=$1", []string{"dQw4w9WgXcQ"})
	if err != nil {
		t.Fatal(err)
	}
	if got := st.args[1]; got != "contentID=dQw4w9WgXcQ" {
		t.Errorf("substituted to %q", got)
	}
	// A missing argument must not leave the literal token behind.
	st, err = parseStep("text $1", nil)
	if err == nil {
		t.Errorf("an unsubstituted step with no text should fail, got %+v", st)
	}
	if got := substitute("search $*", []string{"blade", "runner"}); got != "search blade runner" {
		t.Errorf("substitute($*) = %q", got)
	}
}

func TestParseSteps(t *testing.T) {
	got := ParseSteps(" launch youtube ; wait 3s ;; Select ")
	want := []string{"launch youtube", "wait 3s", "Select"}
	if len(got) != len(want) {
		t.Fatalf("got %d steps, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMacroSetRejectsBadSteps(t *testing.T) {
	ms := &MacroSet{Macros: map[string]Macro{}}
	// Validation happens at save time, not halfway through a replay.
	if err := ms.Set("bad", Macro{Steps: []string{"Home", "Frobnicate"}}); err == nil {
		t.Error("a macro with an unknown key should be rejected when set")
	}
	if err := ms.Set("empty", Macro{}); err == nil {
		t.Error("a macro with no steps should be rejected")
	}
	if err := ms.Set("ok", Macro{Steps: []string{"Home", "wait 1s"}}); err != nil {
		t.Errorf("a valid macro was rejected: %v", err)
	}
}

func TestParseParams(t *testing.T) {
	v, err := parseParams([]string{"contentID=abc", "mediaType=movie"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Get("contentID") != "abc" || v.Get("mediaType") != "movie" {
		t.Errorf("parsed to %v", v)
	}
	if _, err := parseParams([]string{"notakeyvalue"}); err == nil {
		t.Error("a token without = should be rejected")
	}
}
