package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseLibraryFindsOwnedM4B(t *testing.T) {
	page := `<section class="account-list-item"><h3 class="h4">Dune</h3><img class="book-cover" alt="View audiobook of Dune by Frank Herbert"><a href="/user/library/9780441172719/download?file_type=zip">MP3</a><a href="/user/library/9780441172719/download?file_type=m4b">M4B</a></section>`
	books := parseLibrary(page)
	if len(books) != 1 || books[0].ID != "9780441172719" || books[0].Author != "Frank Herbert" || !strings.Contains(books[0].M4BURL, "m4b") {
		t.Fatalf("unexpected parse: %#v", books)
	}
}
func TestEmptyOwnedLibraryIsAValidSearchResult(t *testing.T) {
	if books := parseLibrary(`<main><h1>Your library</h1></main>`); len(books) != 0 {
		t.Fatalf("empty library parsed as %#v", books)
	}
	s := server{syncedAt: time.Now().UTC(), books: []book{}}
	req := httptest.NewRequest(http.MethodPost, "/search", bytes.NewBufferString(`{"query":"Dune","book":{"book_type":"audiobook"}}`))
	response := httptest.NewRecorder()
	s.search(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("empty owned library returned %d: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Results) != 0 {
		t.Fatalf("got %d unexpected results", len(payload.Results))
	}
}
func TestSearchAcceptsShelfarrMetadataFields(t *testing.T) {
	s := server{syncedAt: time.Now().UTC(), books: []book{}}
	req := httptest.NewRequest(http.MethodPost, "/search", bytes.NewBufferString(`{"query":"Dune Frank Herbert","request":{"id":1,"language":"en"},"book":{"id":1,"title":"Dune","author":"Frank Herbert","book_type":"audiobook","year":1965,"language":"en","isbn":"9780441172719","open_library_work_id":"OL1W"}}`))
	response := httptest.NewRecorder()
	s.search(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("Shelfarr payload returned %d: %s", response.Code, response.Body.String())
	}
}
func TestSignedURLsCannotBeChanged(t *testing.T) {
	s := server{config: config{signingKey: "01234567890123456789012345678901"}}
	if s.sign("123456", 1) == s.sign("123457", 1) || s.sign("123456", 1) == s.sign("123456", 2) {
		t.Fatal("signature must bind both ID and expiry")
	}
}
func TestAbsoluteLibroURLRejectsOtherHosts(t *testing.T) {
	if _, err := absoluteLibroURL("https://example.com/file"); err == nil {
		t.Fatal("expected non-Libro host rejection")
	}
}
