package main

// go-webdav v0.7 has no hook for extra collection properties, so it answers
// Apple's calendar-color and CalendarServer's getctag — both requested by
// BusyCal on every poll — with 404. davCalendarProps rewrites PROPFIND
// responses for our calendar collections: it drops those two empty 404
// entries and adds a 200 propstat carrying the real values.
//
// The rewrite matches go-webdav's exact serialization (pinned in go.mod and
// locked by TestDAVCalendarColorAndCTag). If that output ever changes the
// patterns stop matching and responses pass through untouched — clients
// just lose colors and fall back to ETag listing.

import (
	"bytes"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/arthursoares/things-cloud-sdk/thingsdav"
)

const (
	davEmptyColor = `<calendar-color xmlns="http://apple.com/ns/ical/"></calendar-color>`
	davEmptyCTag  = `<getctag xmlns="http://calendarserver.org/ns/"></getctag>`
	davEmpty404   = `<propstat xmlns="DAV:"><prop xmlns="DAV:"></prop><status>HTTP/1.1 404 Not Found</status></propstat>`
	davResponseOp = `<response xmlns="DAV:">`
)

type davRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *davRecorder) Header() http.Header         { return r.header }
func (r *davRecorder) WriteHeader(code int)        { r.status = code }
func (r *davRecorder) Write(p []byte) (int, error) { return r.body.Write(p) }

func davCalendarProps(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" {
			next.ServeHTTP(w, r)
			return
		}
		rec := &davRecorder{header: http.Header{}, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		body := rec.body.Bytes()
		if rec.status == http.StatusMultiStatus {
			if view, err := loadDAVView(); err == nil {
				body = injectCalendarProps(body, view)
			}
		}
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(rec.status)
		w.Write(body)
	})
}

// injectCalendarProps fills in calendar-color and getctag for every
// calendar <response> in a go-webdav multistatus body.
func injectCalendarProps(body []byte, view *thingsdav.View) []byte {
	s := string(body)
	if !strings.Contains(s, davEmptyColor) && !strings.Contains(s, davEmptyCTag) {
		return body
	}
	parts := strings.Split(s, davResponseOp)
	for i := 1; i < len(parts); i++ {
		parts[i] = injectIntoResponse(parts[i], view)
	}
	return []byte(strings.Join(parts, davResponseOp))
}

// injectIntoResponse rewrites one response's contents (everything after the
// opening <response> tag).
func injectIntoResponse(resp string, view *thingsdav.View) string {
	hrefStart := strings.Index(resp, "<href>")
	hrefEnd := strings.Index(resp, "</href>")
	if hrefStart != 0 || hrefEnd < 0 {
		return resp
	}
	href := html.UnescapeString(resp[len("<href>"):hrefEnd])
	calID, name, ok := davSplitPath(href)
	if !ok || name != "" {
		return resp
	}
	cal := view.Calendar(strings.TrimSuffix(calID, "/"))
	if cal == nil {
		return resp
	}

	var found strings.Builder
	if strings.Contains(resp, davEmptyColor) && cal.Color != "" {
		resp = strings.Replace(resp, davEmptyColor, "", 1)
		found.WriteString(`<calendar-color xmlns="http://apple.com/ns/ical/">` + cal.Color + `</calendar-color>`)
	}
	if strings.Contains(resp, davEmptyCTag) && cal.CTag != "" {
		resp = strings.Replace(resp, davEmptyCTag, "", 1)
		found.WriteString(`<getctag xmlns="http://calendarserver.org/ns/">` + cal.CTag + `</getctag>`)
	}
	if found.Len() == 0 {
		return resp
	}
	resp = strings.Replace(resp, davEmpty404, "", 1)
	ok200 := `<propstat xmlns="DAV:"><prop xmlns="DAV:">` + found.String() +
		`</prop><status>HTTP/1.1 200 OK</status></propstat>`
	cut := hrefEnd + len("</href>")
	return resp[:cut] + ok200 + resp[cut:]
}
