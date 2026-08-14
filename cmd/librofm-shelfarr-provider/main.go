// librofm-shelfarr-provider exposes a Shelfarr Custom Acquisition Provider for
// audiobooks that the configured Libro.fm account already owns.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const libroBaseURL = "https://libro.fm"

// Libro.fm rejects non-browser user agents before returning the Rails login
// form. This is the same normal browser identification used by the reference
// sync tools; credentials are still sent only to libro.fm over HTTPS.
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"

type config struct {
	username, password, bearerToken, signingKey, publicBaseURL, listenAddr string
	stateDir                                                               string
	downloadTTL, syncInterval, manualSyncMin                               time.Duration
	syncOnStart                                                            bool
}

func loadConfig() (config, error) {
	c := config{username: os.Getenv("LIBROFM_USERNAME"), password: os.Getenv("LIBROFM_PASSWORD"), bearerToken: os.Getenv("PROVIDER_BEARER_TOKEN"), signingKey: os.Getenv("DOWNLOAD_SIGNING_KEY"), publicBaseURL: strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/"), listenAddr: env("LISTEN_ADDR", ":8080"), stateDir: env("STATE_DIR", "/state"), downloadTTL: 15 * time.Minute, syncInterval: 24 * time.Hour, manualSyncMin: 6 * time.Hour, syncOnStart: envBool("SYNC_ON_START", true)}
	if c.username == "" || c.password == "" {
		return c, errors.New("LIBROFM_USERNAME and LIBROFM_PASSWORD are required")
	}
	if len(c.signingKey) < 32 {
		return c, errors.New("DOWNLOAD_SIGNING_KEY must be at least 32 characters")
	}
	if u, err := url.Parse(c.publicBaseURL); err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return c, errors.New("PUBLIC_BASE_URL must be an absolute http(s) URL")
	}
	if raw := os.Getenv("DOWNLOAD_URL_TTL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 || d > time.Hour {
			return c, errors.New("DOWNLOAD_URL_TTL must be a positive duration no greater than 1h")
		}
		c.downloadTTL = d
	}
	if raw := os.Getenv("SYNC_INTERVAL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 6*time.Hour || d > 7*24*time.Hour {
			return c, errors.New("SYNC_INTERVAL must be between 6h and 168h")
		}
		c.syncInterval = d
	}
	if raw := os.Getenv("MANUAL_SYNC_MIN_INTERVAL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < time.Hour || d > c.syncInterval {
			return c, errors.New("MANUAL_SYNC_MIN_INTERVAL must be between 1h and SYNC_INTERVAL")
		}
		c.manualSyncMin = d
	}
	return c, nil
}
func envBool(key string, fallback bool) bool {
	value, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	return value == "1" || strings.EqualFold(value, "true") || strings.EqualFold(value, "yes")
}
func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

type server struct {
	config      config
	httpClient  *http.Client
	stateMu     sync.RWMutex
	refreshMu   sync.Mutex
	books       []book
	syncedAt    time.Time
	lastAttempt time.Time
}
type book struct{ ID, Title, Author, M4BURL string }
type libraryCache struct {
	SyncedAt    time.Time `json:"synced_at"`
	LastAttempt time.Time `json:"last_attempt"`
	Books       []book    `json:"books"`
}
type searchRequest struct {
	Query string `json:"query"`
	Book  struct {
		Title    string `json:"title"`
		Author   string `json:"author"`
		BookType string `json:"book_type"`
		ISBN     string `json:"isbn"`
	} `json:"book"`
}
type acquireRequest struct {
	ProviderResultID string `json:"provider_result_id"`
	Result           struct {
		ProviderResultID string `json:"provider_result_id"`
	} `json:"result"`
}

func main() {
	c, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	s := &server{config: c, httpClient: &http.Client{Timeout: 45 * time.Second}}
	if err := s.loadCache(); err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("POST /search", s.auth(s.search))
	mux.HandleFunc("POST /acquire", s.auth(s.acquire))
	mux.HandleFunc("POST /sync", s.auth(s.syncNow))
	mux.HandleFunc("GET /sync-status", s.auth(s.syncStatus))
	mux.HandleFunc("GET /download/", s.download)
	go s.syncLoop()
	log.Printf("starting Libro.fm Shelfarr provider on %s", c.listenAddr)
	log.Fatal(http.ListenAndServe(c.listenAddr, securityHeaders(mux)))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.config.bearerToken != "" && subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), []byte(s.config.bearerToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}
func (s *server) syncStatus(w http.ResponseWriter, _ *http.Request) {
	_, syncedAt := s.snapshot()
	jsonResponse(w, http.StatusOK, map[string]any{"synced_at": syncedAt, "ready": !syncedAt.IsZero()})
}
func (s *server) syncNow(w http.ResponseWriter, r *http.Request) {
	lastAttempt := s.lastSyncAttempt()
	if !lastAttempt.IsZero() && time.Since(lastAttempt) < s.config.manualSyncMin {
		retryAfter := time.Until(lastAttempt.Add(s.config.manualSyncMin)).Round(time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
		http.Error(w, "manual sync is rate limited; retry after "+retryAfter.String(), http.StatusTooManyRequests)
		return
	}
	if err := s.refreshLibrary(); err != nil {
		providerError(w, err)
		return
	}
	s.syncStatus(w, r)
}

func (s *server) search(w http.ResponseWriter, r *http.Request) {
	var request searchRequest
	if err := decodeJSON(r, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if request.Book.BookType != "" && request.Book.BookType != "audiobook" {
		jsonResponse(w, http.StatusOK, map[string]any{"results": []any{}})
		return
	}
	books, syncedAt := s.snapshot()
	if syncedAt.IsZero() {
		http.Error(w, "owned-library cache is not ready; retry after the initial sync", http.StatusServiceUnavailable)
		return
	}
	needle := normalize(strings.Join([]string{request.Query, request.Book.Title, request.Book.Author, request.Book.ISBN}, " "))
	results := make([]map[string]any, 0)
	for _, b := range books {
		if b.M4BURL == "" || !matches(b, needle) {
			continue
		}
		results = append(results, map[string]any{"id": b.ID, "title": b.Title, "author": b.Author, "format": "m4b", "download_type": "direct", "availability": "available"})
	}
	sort.Slice(results, func(i, j int) bool { return results[i]["title"].(string) < results[j]["title"].(string) })
	jsonResponse(w, http.StatusOK, map[string]any{"results": results})
}

func (s *server) acquire(w http.ResponseWriter, r *http.Request) {
	var request acquireRequest
	if err := decodeJSON(r, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := request.ProviderResultID
	if id == "" {
		id = request.Result.ProviderResultID
	}
	if !validID(id) {
		http.Error(w, "missing or invalid provider_result_id", http.StatusBadRequest)
		return
	}
	books, syncedAt := s.snapshot()
	if syncedAt.IsZero() {
		http.Error(w, "owned-library cache is not ready; retry after the initial sync", http.StatusServiceUnavailable)
		return
	}
	found := false
	for _, b := range books {
		if b.ID == id && b.M4BURL != "" {
			found = true
			break
		}
	}
	if !found {
		http.Error(w, "owned M4B result no longer available", http.StatusNotFound)
		return
	}
	expires := time.Now().Add(s.config.downloadTTL).Unix()
	token := s.sign(id, expires)
	jsonResponse(w, http.StatusOK, map[string]string{"download_type": "direct", "direct_url": fmt.Sprintf("%s/download/%s?expires=%d&signature=%s", s.config.publicBaseURL, url.PathEscape(id), expires, token)})
}

func (s *server) download(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/download/")
	expires, err := strconv.ParseInt(r.URL.Query().Get("expires"), 10, 64)
	if err != nil || !validID(id) || time.Now().Unix() > expires || !hmac.Equal([]byte(r.URL.Query().Get("signature")), []byte(s.sign(id, expires))) {
		http.Error(w, "invalid or expired download URL", http.StatusForbidden)
		return
	}
	books, _ := s.snapshot()
	var source string
	for _, b := range books {
		if b.ID == id {
			source = b.M4BURL
			break
		}
	}
	if source == "" {
		http.Error(w, "owned M4B result no longer available", http.StatusNotFound)
		return
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 0}
	if err := s.login(client); err != nil {
		providerError(w, err)
		return
	}
	u, err := absoluteLibroURL(source)
	if err != nil {
		providerError(w, err)
		return
	}
	response, err := client.Get(u)
	if err != nil {
		providerError(w, fmt.Errorf("fetch audiobook: %w", err))
		return
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		providerError(w, fmt.Errorf("Libro.fm download returned %s", response.Status))
		return
	}
	w.Header().Set("Content-Type", "audio/mp4")
	if value := response.Header.Get("Content-Length"); value != "" {
		w.Header().Set("Content-Length", value)
	}
	if value := response.Header.Get("Content-Disposition"); value != "" {
		w.Header().Set("Content-Disposition", value)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, response.Body)
}

func (s *server) fetchLibrary() ([]book, error) {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 45 * time.Second}
	page, err := s.loginAndLibrary(client)
	if err != nil {
		return nil, err
	}
	books := parseLibrary(page)
	seen := map[string]bool{}
	for _, b := range books {
		seen[b.ID] = true
	}
	for pageNumber := 2; ; pageNumber++ {
		next, err := client.Get(fmt.Sprintf("%s/user/library?page=%d", libroBaseURL, pageNumber))
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(next.Body, 8<<20))
		next.Body.Close()
		if readErr != nil || next.StatusCode/100 != 2 {
			break
		}
		parsed := parseLibrary(string(data))
		added := 0
		for _, b := range parsed {
			if !seen[b.ID] {
				books = append(books, b)
				seen[b.ID] = true
				added++
			}
		}
		if added == 0 {
			break
		}
	}
	return books, nil
}

func (s *server) cachePath() string { return filepath.Join(s.config.stateDir, "library.json") }
func (s *server) loadCache() error {
	data, err := os.ReadFile(s.cachePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read owned-library cache: %w", err)
	}
	var cache libraryCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return fmt.Errorf("decode owned-library cache: %w", err)
	}
	if cache.SyncedAt.IsZero() && cache.LastAttempt.IsZero() {
		return errors.New("owned-library cache has no sync timestamps")
	}
	s.books, s.syncedAt, s.lastAttempt = cache.Books, cache.SyncedAt, cache.LastAttempt
	log.Printf("loaded owned-library cache: %d titles, synced %s", len(cache.Books), cache.SyncedAt.Format(time.RFC3339))
	return nil
}
func (s *server) snapshot() ([]book, time.Time) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return append([]book(nil), s.books...), s.syncedAt
}
func (s *server) lastSyncAttempt() time.Time {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.lastAttempt
}
func (s *server) writeCache(cache libraryCache) error {
	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.config.stateDir, 0700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	temporary, err := os.CreateTemp(s.config.stateDir, ".library.json-")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0600); err == nil {
		_, err = temporary.Write(data)
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write owned-library cache: %w", err)
	}
	if err := os.Rename(name, s.cachePath()); err != nil {
		return fmt.Errorf("publish owned-library cache: %w", err)
	}
	return nil
}
func (s *server) refreshLibrary() error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	existing, syncedAt := s.snapshot()
	attemptedAt := time.Now().UTC()
	if err := s.writeCache(libraryCache{SyncedAt: syncedAt, LastAttempt: attemptedAt, Books: existing}); err != nil {
		return err
	}
	s.stateMu.Lock()
	s.lastAttempt = attemptedAt
	s.stateMu.Unlock()
	books, err := s.fetchLibrary()
	if err != nil {
		return err
	}
	cache := libraryCache{SyncedAt: time.Now().UTC(), LastAttempt: attemptedAt, Books: books}
	if err := s.writeCache(cache); err != nil {
		return err
	}
	s.stateMu.Lock()
	s.books, s.syncedAt, s.lastAttempt = books, cache.SyncedAt, attemptedAt
	s.stateMu.Unlock()
	log.Printf("owned-library sync completed: %d titles", len(books))
	return nil
}
func (s *server) syncLoop() {
	if s.config.syncOnStart && (s.lastSyncAttempt().IsZero() || time.Since(s.lastSyncAttempt()) >= s.config.syncInterval) {
		if err := s.refreshLibrary(); err != nil {
			log.Printf("initial owned-library sync failed: %v", err)
		}
	}
	ticker := time.NewTicker(s.config.syncInterval)
	defer ticker.Stop()
	for range ticker.C {
		if err := s.refreshLibrary(); err != nil {
			log.Printf("scheduled owned-library sync failed: %v", err)
		}
	}
}
func (s *server) loginAndLibrary(client *http.Client) (string, error) {
	if err := s.login(client); err != nil {
		return "", err
	}
	response, err := client.Get(libroBaseURL + "/user/library")
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if response.StatusCode/100 != 2 || strings.Contains(string(data), "Please log in") {
		return "", errors.New("Libro.fm login failed")
	}
	return string(data), nil
}
func (s *server) login(client *http.Client) error {
	request, err := http.NewRequest(http.MethodGet, libroBaseURL+"/login", nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", userAgent)
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	response.Body.Close()
	if err != nil {
		return err
	}
	token := csrfToken(string(data))
	if token == "" {
		return errors.New("Libro.fm login page did not include CSRF token")
	}
	form := url.Values{"authenticity_token": {token}, "email": {s.config.username}, "password": {s.config.password}, "commit": {"Log in"}}
	response, err = client.PostForm(libroBaseURL+"/user/attempt-login", form)
	if err != nil {
		return err
	}
	response.Body.Close()
	return nil
}

var csrfRE = regexp.MustCompile(`(?is)<input[^>]+name=["']authenticity_token["'][^>]+value=["']([^"']+)["']`)
var sectionRE = regexp.MustCompile(`(?is)<section[^>]*class=["'][^"']*account-list-item[^"']*["'][^>]*>(.*?)</section>`)
var titleRE = regexp.MustCompile(`(?is)<h3[^>]*class=["'][^"']*h4[^"']*["'][^>]*>(.*?)</h3>`)
var altRE = regexp.MustCompile(`(?is)<img[^>]*class=["'][^"']*book-cover[^"']*["'][^>]*alt=["']([^"']+)["']`)
var downloadRE = regexp.MustCompile(`(?is)href=["']([^"']*/user/library/(\d+)/download[^"']*)["']`)

func csrfToken(page string) string {
	match := csrfRE.FindStringSubmatch(page)
	if len(match) == 2 {
		return unescape(match[1])
	}
	return ""
}
func parseLibrary(page string) []book {
	books := []book{}
	for _, section := range sectionRE.FindAllStringSubmatch(page, -1) {
		title := text(titleRE.FindStringSubmatch(section[1]))
		downloads := downloadRE.FindAllStringSubmatch(unescape(section[1]), -1)
		if title == "" || len(downloads) == 0 {
			continue
		}
		b := book{Title: title, ID: downloads[0][2]}
		if alt := altRE.FindStringSubmatch(section[1]); len(alt) == 2 {
			author := regexp.MustCompile(`(?i)\bby\s+(.+)$`).FindStringSubmatch(unescape(alt[1]))
			if len(author) == 2 {
				b.Author = strings.TrimSpace(author[1])
			}
		}
		for _, d := range downloads {
			if strings.Contains(d[1], "file_type=m4b") {
				b.M4BURL = d[1]
				break
			}
		}
		books = append(books, b)
	}
	return books
}
func text(value []string) string {
	if len(value) != 2 {
		return ""
	}
	return strings.TrimSpace(unescape(regexp.MustCompile(`(?s)<[^>]+>`).ReplaceAllString(value[1], "")))
}
func unescape(value string) string {
	replacer := strings.NewReplacer("&amp;", "&", "&quot;", "\"", "&#39;", "'", "&apos;", "'", "&lt;", "<", "&gt;", ">")
	return replacer.Replace(value)
}
func normalize(value string) string { return strings.Join(strings.Fields(strings.ToLower(value)), " ") }
func matches(b book, needle string) bool {
	haystack := normalize(b.Title + " " + b.Author + " " + b.ID)
	tokens := strings.Fields(needle)
	matched := 0
	for _, token := range tokens {
		if len(token) > 2 && strings.Contains(haystack, token) {
			matched++
		}
	}
	return matched > 0 && (matched >= 2 || len(tokens) == 1)
}
func validID(value string) bool { return regexp.MustCompile(`^[0-9]{6,20}$`).MatchString(value) }
func absoluteLibroURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	resolved := (&url.URL{Scheme: "https", Host: "libro.fm"}).ResolveReference(u)
	if resolved.Scheme != "https" || resolved.Host != "libro.fm" {
		return "", errors.New("refusing non-Libro.fm download URL")
	}
	return resolved.String(), nil
}
func (s *server) sign(id string, expires int64) string {
	mac := hmac.New(sha256.New, []byte(s.config.signingKey))
	fmt.Fprintf(mac, "%s:%d", id, expires)
	return hex.EncodeToString(mac.Sum(nil))
}
func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func providerError(w http.ResponseWriter, err error) {
	log.Printf("provider error: %v", err)
	http.Error(w, "provider could not access Libro.fm; check server logs", http.StatusBadGateway)
}
