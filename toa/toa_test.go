package toa

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMatchKeys(t *testing.T) {
	// Shapes taken from the live API (2526-AUS-CMP, 2526-FIM-CMP).
	assert.Equal(t, "2526-AUS-CMP-Q003-1", QualMatchKey("2526-AUS-CMP", 3))
	assert.Equal(t, "2526-AUS-CMP-Q042-1", QualMatchKey("2526-AUS-CMP", 42))
	assert.Equal(t, "2526-AUS-CMP-Q123-1", QualMatchKey("2526-AUS-CMP", 123))
	assert.Equal(t, "2526-AUS-CMP-Q1001-1", QualMatchKey("2526-AUS-CMP", 1001))
	assert.Equal(t, "2526-AUS-CMP-E101-1", PlayoffMatchKey("2526-AUS-CMP", 1, 1))
	assert.Equal(t, "2526-AUS-CMP-E1001-1", PlayoffMatchKey("2526-AUS-CMP", 10, 1))
	assert.Equal(t, "2526-AUS-CMP-E1401-1", PlayoffMatchKey("2526-AUS-CMP", 14, 1))
	assert.Equal(t, "2526-FIM-CMP-E103-1", PlayoffMatchKey("2526-FIM-CMP", 1, 3))
	assert.Equal(t, "2526-FIM-CMP-E901-1", PlayoffMatchKey("2526-FIM-CMP", 0, 1))
	assert.Equal(t, "2526-FIM-CMP-E112-1", PlayoffMatchKey("2526-FIM-CMP", 1, 12))

	// No event key or no number means no key, never a half-built one.
	assert.Equal(t, "", QualMatchKey("", 3))
	assert.Equal(t, "", QualMatchKey("2526-AUS-CMP", 0))
	assert.Equal(t, "", PlayoffMatchKey("", 1, 1))
	assert.Equal(t, "", PlayoffMatchKey("2526-AUS-CMP", 1, 0))
}

func TestVideoURL(t *testing.T) {
	assert.Equal(t, "https://www.youtube.com/watch?v=dQw4w9WgXcQ", VideoURL("dQw4w9WgXcQ"))
}

// fakeTOA records requests and answers the PUT with modified, and the GET with
// stored (nil = 404).
type fakeTOA struct {
	newAPI   bool // answer matched_count + unknown_match_keys like TOA-API since 2026-10
	matched  int
	unknown  []string
	modified int
	stored   *string
	status   int
	puts     []*http.Request
	putBody  []byte
	gets     []string
}

func (f *fakeTOA) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			f.puts = append(f.puts, r)
			f.putBody, _ = io.ReadAll(r.Body)
			if f.status != 0 {
				w.WriteHeader(f.status)
				_, _ = w.Write([]byte(`{"_code":` + strconv.Itoa(f.status) + `,"_message":"This event key is for a different event."}`))
				return
			}
			if f.newAPI {
				unknown := f.unknown
				if unknown == nil {
					unknown = []string{}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"success": true, "matched_count": f.matched, "modified_count": f.modified,
					"unknown_match_keys": unknown,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": 1, "inserted_count": 0, "modified_count": f.modified, "deleted_count": 0,
			})
		case http.MethodGet:
			f.gets = append(f.gets, r.URL.Path)
			assert.Equal(t, ApplicationOrigin, r.Header.Get("X-Application-Origin"))
			assert.Equal(t, "level3key", r.Header.Get("X-TOA-Key"))
			if f.stored == nil {
				w.WriteHeader(404)
				_, _ = w.Write([]byte(`{"code":404,"message":"Content not found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"match_key": "x", "video_url": *f.stored}})
		}
	}
}

func TestSubmitMatchVideoRequestShape(t *testing.T) {
	f := &fakeTOA{modified: 1}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	err := SubmitMatchVideo(srv.URL+"/", "level3key", "2627-FIM-TEST-Q003-1", VideoURL("abcdefghijk"))
	assert.NoError(t, err)
	if assert.Len(t, f.puts, 1) {
		r := f.puts[0]
		assert.Equal(t, "/api/match/video", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, ApplicationOrigin, r.Header.Get("X-Application-Origin"))
		assert.Equal(t, "level3key", r.Header.Get("X-TOA-Key"))
	}
	assert.JSONEq(t,
		`[{"match_key":"2627-FIM-TEST-Q003-1","video_url":"https://www.youtube.com/watch?v=abcdefghijk"}]`,
		string(f.putBody))
	assert.Empty(t, f.gets, "a write that changed a match needs no read-back")
}

func TestSubmitMatchVideoErrors(t *testing.T) {
	// TOA's permission error is a 400 with a JSON message; it comes back whole.
	f := &fakeTOA{status: 403}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	err := SubmitMatchVideo(srv.URL, "level3key", "2627-FIM-TEST-Q003-1", VideoURL("abcdefghijk"))
	if assert.Error(t, err) {
		assert.Equal(t, "TOA 403: This event key is for a different event.", err.Error())
	}

	// Missing inputs never reach the network.
	assert.Error(t, SubmitMatchVideo(srv.URL, "", "k", "u"))
	assert.Error(t, SubmitMatchVideo(srv.URL, "level3key", "", "u"))
	assert.Len(t, f.puts, 1)
}

func TestSubmitMatchVideoNothingModified(t *testing.T) {
	url := VideoURL("abcdefghijk")

	// Already set to this URL: a resubmit, so success.
	f := &fakeTOA{modified: 0, stored: &url}
	srv := httptest.NewServer(f.handler(t))
	assert.NoError(t, SubmitMatchVideo(srv.URL, "level3key", "2627-FIM-TEST-Q003-1", url))
	assert.Equal(t, []string{"/api/match/2627-FIM-TEST-Q003-1"}, f.gets)
	srv.Close()

	// Match exists with another URL: the write went nowhere.
	other := VideoURL("zzzzzzzzzzz")
	f = &fakeTOA{modified: 0, stored: &other}
	srv = httptest.NewServer(f.handler(t))
	assert.Error(t, SubmitMatchVideo(srv.URL, "level3key", "2627-FIM-TEST-Q003-1", url))
	srv.Close()

	// No such match.
	f = &fakeTOA{modified: 0}
	srv = httptest.NewServer(f.handler(t))
	err := SubmitMatchVideo(srv.URL, "level3key", "2627-FIM-TEST-Q099-1", url)
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "changed no match")
	}
	srv.Close()
}

func TestSubmitMatchVideoNewAPI(t *testing.T) {
	url := VideoURL("abcdefghijk")

	// Changed, or already this URL (matched but not modified): success, no read.
	for _, f := range []*fakeTOA{
		{newAPI: true, matched: 1, modified: 1},
		{newAPI: true, matched: 1, modified: 0},
	} {
		srv := httptest.NewServer(f.handler(t))
		assert.NoError(t, SubmitMatchVideo(srv.URL, "level3key", "2627-FIM-TEST-Q003-1", url))
		assert.Empty(t, f.gets)
		srv.Close()
	}

	// TOA names the key as unknown: a clear error, no read.
	f := &fakeTOA{newAPI: true, unknown: []string{"2627-FIM-TEST-Q099-1"}}
	srv := httptest.NewServer(f.handler(t))
	err := SubmitMatchVideo(srv.URL, "level3key", "2627-FIM-TEST-Q099-1", url)
	if assert.Error(t, err) {
		assert.Equal(t, "TOA has no match 2627-FIM-TEST-Q099-1", err.Error())
	}
	assert.Empty(t, f.gets)
	srv.Close()
}
