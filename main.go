package icalmiddleware

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Config struct {
	ForwardToken bool   `json:"forwardToken,omitempty"`
	Freshness    int64  `json:"freshness,omitempty"`
	HeaderName   string `json:"headerName,omitempty"`
}

func CreateConfig() *Config {
	return &Config{
		HeaderName:   "Authorization",
		ForwardToken: false,
		Freshness:    3600,
	}
}

type ICalMiddleware struct {
	next         http.Handler
	headerName   string
	forwardToken bool
	freshness    int64
	cache        map[string]time.Time
	name         string
}

func New(_ context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	return &ICalMiddleware{
		headerName:   config.HeaderName,
		forwardToken: config.ForwardToken,
		freshness:    config.Freshness,
		next:         next,
		cache:        make(map[string]time.Time),
		name:         name,
	}, nil
}

func (plugin *ICalMiddleware) getCached(key string) bool {
	item, found := plugin.cache[key]
	if found && time.Now().Before(item) {
		return true
	}
	return false
}

func (plugin *ICalMiddleware) setCache(key string) {
	plugin.cache[key] = time.Now().Add(time.Duration(plugin.freshness) * time.Second)
}

func (plugin *ICalMiddleware) httpRequestAndCache(url string) error {
	response, err := http.Get("https://ical.psu.ru/calendars/" + url)
	if err != nil {
		return fmt.Errorf("request error: %v", err)
	}
	defer response.Body.Close()

	body := make([]byte, 20)

	_, err = io.ReadAtLeast(response.Body, body, 20)
	if err != nil {
		return fmt.Errorf("read error: %v", err)
	}

	result := string(body)

	if strings.HasPrefix(result, "BEGIN") {
		plugin.setCache(url)
		fmt.Println("Request valid")
	} else {
		fmt.Println("Request invalid")
		return fmt.Errorf("request invalid")
	}

	return nil
}

// extractTokenFromHeader extracts the token from the header. If the token is found, it is removed from the header unless forwardToken is true.
func (plugin *ICalMiddleware) extractTokenFromHeader(request *http.Request) string {
	header, ok := request.Header[plugin.headerName]
	if !ok {
		return ""
	}

	token := header[0]

	if !plugin.forwardToken {
		request.Header.Del(plugin.headerName)
	}

	if strings.HasPrefix(token, "Bearer ") {
		return token[7:]
	}
	return token
}

// validate validates the request and returns the HTTP status code or an error if the request is not valid. It also sets any headers that should be forwarded to the backend.
func (plugin *ICalMiddleware) validate(request *http.Request) (int, error) {
	token := plugin.extractTokenFromHeader(request)
	if token == "" {
		// No token provided
		return http.StatusUnauthorized, fmt.Errorf("no token provided")
	} else if !plugin.getCached(token) {
		// Token provided
		err := plugin.httpRequestAndCache(token)
		if err != nil {
			return http.StatusUnauthorized, err
		}
	}
	return http.StatusOK, nil
}

func (a *ICalMiddleware) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	_, err := a.validate(req)
	if err != nil {
		http.Error(rw, "Unauthorized. Attach valid ICal ETIS token in "+a.headerName+" header", http.StatusUnauthorized)
		return
	}
	a.next.ServeHTTP(rw, req)
}
