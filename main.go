package icalmiddleware

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

type Config struct {
	ForwardToken bool   `json:"forwardToken,omitempty"`
	Freshness    int64  `json:"freshness,omitempty"`
	HeaderName   string `json:"headerName,omitempty"`
	AllowSubnet  string `json:"allowSubnet,omitempty"`
}

func CreateConfig() *Config {
	return &Config{
		HeaderName:   "Authorization",
		ForwardToken: false,
		Freshness:    3600,
		AllowSubnet:  "0.0.0.0/24",
	}
}

type ICalMiddleware struct {
	next         http.Handler
	headerName   string
	forwardToken bool
	freshness    int64
	cache        *Cache
	allowSubnet  netip.Prefix
	name         string
}

func New(_ context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	network, err := netip.ParsePrefix(config.AllowSubnet)
	if err != nil {
		return nil, fmt.Errorf("subnet parse error: %v", err)
	}

	cache := NewCache(time.Duration(config.Freshness)*time.Second, 8*time.Hour)

	return &ICalMiddleware{
		headerName:   config.HeaderName,
		forwardToken: config.ForwardToken,
		freshness:    config.Freshness,
		allowSubnet:  network,
		next:         next,
		cache:        cache,
		name:         name,
	}, nil
}

func (plugin *ICalMiddleware) setCache(key string) {
	plugin.cache.Set(key, true, 0)
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

func ReadUserIP(r *http.Request) string {
	IPAddress := r.Header.Get("X-Real-Ip")
	if IPAddress == "" {
		IPAddress = r.Header.Get("X-Forwarded-For")
	}
	if IPAddress == "" {
		IPAddress, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	return IPAddress
}

func (plugin *ICalMiddleware) containsSubnet(address string) bool {
	ip, err := netip.ParseAddr(address)
	if err != nil {
		fmt.Printf("Invalid addr: %v", err)
		return false
	}
	fmt.Printf("%v contains %v %v\n", ip, plugin.allowSubnet, plugin.allowSubnet.Contains(ip))
	return plugin.allowSubnet.Contains(ip)
}

// validate validates the request and returns the HTTP status code or an error if the request is not valid. It also sets any headers that should be forwarded to the backend.
func (plugin *ICalMiddleware) validate(request *http.Request) (int, error) {
	if !plugin.containsSubnet(ReadUserIP(request)) {
		token := plugin.extractTokenFromHeader(request)
		if token == "" {
			// No token provided
			fmt.Println("No token provided")
			return http.StatusUnauthorized, fmt.Errorf("no token provided")
		} else if !(plugin.cache.Has(token)) {
			// Token provided
			err := plugin.httpRequestAndCache(token)
			if err != nil {
				return http.StatusUnauthorized, err
			}
		}
		fmt.Println("Token found in cache")
	}
	return http.StatusOK, nil
}

func (plugin *ICalMiddleware) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	_, err := plugin.validate(req)
	if err != nil {
		origin := req.Header.Get("Origin")
		if origin != "" {
			rw.Header().Add("Cache-Control", "no-cache")
			rw.Header().Add("Access-Control-Allow-Origin", origin)
			rw.Header().Add("Access-Control-Allow-Headers", "*")
			rw.Header().Add("Access-Control-Max-Age", "0")
		}
		http.Error(rw, "Unauthorized. Attach valid ICal ETIS token in "+plugin.headerName+" header", http.StatusUnauthorized)
		return
	}
	plugin.next.ServeHTTP(rw, req)
}
