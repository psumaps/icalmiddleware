package icalmiddleware

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// LogLevel represents the level of logging
type LogLevel int

const (
	// LogLevelError only logs errors
	LogLevelError LogLevel = iota
	// LogLevelInfo logs errors and important information
	LogLevelInfo
	// LogLevelDebug logs everything
	LogLevelDebug
)

// Config represents the middleware configuration
type Config struct {
	ForwardToken    bool     `json:"forwardToken,omitempty" yaml:"forwardToken,omitempty" toml:"forwardToken,omitempty"`
	Freshness       int64    `json:"freshness,omitempty" yaml:"freshness,omitempty" toml:"freshness,omitempty"`
	HeaderName      string   `json:"headerName,omitempty" yaml:"headerName,omitempty" toml:"headerName,omitempty"`
	AllowSubnet     []string `json:"allowSubnet,omitempty" yaml:"allowSubnet,omitempty" toml:"allowSubnet,omitempty"`
	Timeout         int64    `json:"timeout,omitempty" yaml:"timeout,omitempty" toml:"timeout,omitempty"`
	GlobalRateLimit int      `json:"globalRateLimit,omitempty" yaml:"globalRateLimit,omitempty" toml:"globalRateLimit,omitempty"`
	PerIPRateLimit  int      `json:"perIPRateLimit,omitempty" yaml:"perIPRateLimit,omitempty" toml:"perIPRateLimit,omitempty"`
	RateLimitWindow int64    `json:"rateLimitWindow,omitempty" yaml:"rateLimitWindow,omitempty" toml:"rateLimitWindow,omitempty"`
	LogLevel        string   `json:"logLevel,omitempty" yaml:"logLevel,omitempty" toml:"logLevel,omitempty"`
	CleanupInterval int64    `json:"cleanupInterval,omitempty" yaml:"cleanupInterval,omitempty" toml:"cleanupInterval,omitempty"`
	UpstreamURL     string   `json:"upstreamURL,omitempty" yaml:"upstreamURL,omitempty" toml:"upstreamURL,omitempty"`
}

// CreateConfig creates a default configuration
func CreateConfig() *Config {
	return &Config{
		HeaderName:      "Authorization",
		ForwardToken:    false,
		Freshness:       3600,
		AllowSubnet:     []string{"0.0.0.0/24"},
		Timeout:         10,
		GlobalRateLimit: 100,
		PerIPRateLimit:  10,
		RateLimitWindow: 60,
		LogLevel:        "info",
		CleanupInterval: 300, // 5 minutes
		UpstreamURL:     "https://ical.psu.ru/calendars/",
	}
}

// RequestMultiplexer represents an in-flight request for a specific token
type RequestMultiplexer struct {
	done      chan struct{} // Channel to signal when the request is complete
	err       error         // Error from the request, if any
	isRunning bool          // Flag to indicate if the request is currently running
	timestamp time.Time     // When the request was started
}

// ICalMiddleware is the main middleware struct
type ICalMiddleware struct {
	next         http.Handler
	headerName   string
	forwardToken bool
	freshness    int64
	cache        *Cache
	allowSubnet  []netip.Prefix
	timeout      time.Duration
	name         string
	httpClient   *http.Client
	logLevel     LogLevel
	upstreamURL  string

	// Rate limiting fields
	globalLimiter   *rate.Limiter
	ipLimiters      map[string]*rate.Limiter
	ipLimiterMutex  sync.RWMutex
	perIPRateLimit  int
	rateLimitWindow time.Duration
	cleanupInterval time.Duration
	lastCleanup     time.Time

	// Multiplexing fields
	inFlightRequests     map[string]*RequestMultiplexer
	inFlightRequestMutex sync.RWMutex
}

// parseLogLevel converts a string log level to LogLevel
func parseLogLevel(level string) LogLevel {
	switch strings.ToLower(level) {
	case "debug":
		return LogLevelDebug
	case "error":
		return LogLevelError
	default:
		return LogLevelInfo
	}
}

// New creates a new ICalMiddleware instance
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	var cidrs []netip.Prefix
	for _, cidr := range config.AllowSubnet {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			fmt.Printf("[ERROR] [%s] Invalid subnet '%s': %v\n", name, cidr, err)
			continue
		}
		cidrs = append(cidrs, prefix)
	}
	if len(cidrs) == 0 {
		return nil, fmt.Errorf("no valid subnets provided")
	}

	timeout := time.Duration(config.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	
	// Create a custom HTTP client with proper timeouts
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialer := &net.Dialer{
				Timeout:   timeout / 3,
				KeepAlive: 30 * time.Second,
			}
			return dialer.DialContext(ctx, network, addr)
		},
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   timeout / 3,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: timeout / 2,
		MaxIdleConnsPerHost:   10,
		DisableCompression:    false,
	}
	
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
	
	cache := NewCache(time.Duration(config.Freshness)*time.Second, 8*time.Hour)

	var globalLimiter *rate.Limiter
	if config.GlobalRateLimit > 0 && config.RateLimitWindow > 0 {
		ratePerSec := float64(config.GlobalRateLimit) / float64(config.RateLimitWindow)
		globalLimiter = rate.NewLimiter(rate.Limit(ratePerSec), config.GlobalRateLimit)
	}
	
	cleanupInterval := time.Duration(config.CleanupInterval) * time.Second
	if cleanupInterval <= 0 {
		cleanupInterval = 5 * time.Minute
	}

	upstreamURL := config.UpstreamURL
	if upstreamURL == "" {
		upstreamURL = "https://ical.psu.ru/calendars/"
	}

	return &ICalMiddleware{
		headerName:          config.HeaderName,
		forwardToken:        config.ForwardToken,
		freshness:           config.Freshness,
		allowSubnet:         cidrs,
		next:                next,
		cache:               cache,
		timeout:             timeout,
		name:                name,
		httpClient:          httpClient,
		logLevel:            parseLogLevel(config.LogLevel),
		globalLimiter:       globalLimiter,
		ipLimiters:          make(map[string]*rate.Limiter),
		perIPRateLimit:      config.PerIPRateLimit,
		rateLimitWindow:     time.Duration(config.RateLimitWindow) * time.Second,
		inFlightRequests:    make(map[string]*RequestMultiplexer),
		cleanupInterval:     cleanupInterval,
		lastCleanup:         time.Now(),
		upstreamURL:         upstreamURL,
	}, nil
}

// log logs a message with the given level
func (plugin *ICalMiddleware) log(level LogLevel, format string, args ...interface{}) {
	if level <= plugin.logLevel {
		var levelStr string
		switch level {
		case LogLevelError:
			levelStr = "ERROR"
		case LogLevelInfo:
			levelStr = "INFO"
		case LogLevelDebug:
			levelStr = "DEBUG"
		}
		fmt.Printf("[%s] [%s] %s\n", levelStr, plugin.name, fmt.Sprintf(format, args...))
	}
}

// setCache adds a token to the cache
func (plugin *ICalMiddleware) setCache(key string) {
	plugin.cache.Set(key, true, 0)
}

// httpRequestAndCache makes a request to validate a token and caches the result
func (plugin *ICalMiddleware) httpRequestAndCache(token string) error {
	// Check if the token is already in the cache
	if plugin.cache.Has(token) {
		plugin.log(LogLevelDebug, "Token '%s' already in cache, skipping request", token)
		return nil
	}

	// Check if there's already an in-flight request for this token
	plugin.inFlightRequestMutex.RLock()
	multiplexer, exists := plugin.inFlightRequests[token]
	isRunning := exists && multiplexer.isRunning
	plugin.inFlightRequestMutex.RUnlock()
	
	if isRunning {
		// A request for this token is already in progress
		plugin.log(LogLevelDebug, "Request for token '%s' already in progress, waiting for result", token)
		
		// Wait for the in-flight request to complete
		<-multiplexer.done
		
		// Return the result of the original request
		return multiplexer.err
	}
	
	// Create or reuse a multiplexer
	plugin.inFlightRequestMutex.Lock()
	multiplexer, exists = plugin.inFlightRequests[token]
	if !exists {
		multiplexer = &RequestMultiplexer{
			done:      make(chan struct{}),
			isRunning: true,
			timestamp: time.Now(),
		}
		plugin.inFlightRequests[token] = multiplexer
	} else if !multiplexer.isRunning {
		// Reuse existing multiplexer
		multiplexer.done = make(chan struct{})
		multiplexer.isRunning = true
		multiplexer.timestamp = time.Now()
	}
	plugin.inFlightRequestMutex.Unlock()

	// Make the actual request
	fullURL := plugin.upstreamURL + token
	ctx, cancel := context.WithTimeout(context.Background(), plugin.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		err = fmt.Errorf("error creating request for %s: %w", fullURL, err)
		plugin.log(LogLevelError, err.Error())
		plugin.completeRequest(token, err)
		return err
	}

	// Add standard headers
	req.Header.Set("User-Agent", "Traefik-ICalMiddleware/1.0")
	req.Header.Set("Accept", "text/calendar")

	response, err := plugin.httpClient.Do(req)
	if err != nil {
		err = fmt.Errorf("error making request to %s: %w", fullURL, err)
		plugin.log(LogLevelError, err.Error())
		plugin.completeRequest(token, err)
		return err
	}
	defer response.Body.Close()

	// Upstream always returns http.StatusOK, so we need to check the content
	body := make([]byte, 20)
	_, err = io.ReadAtLeast(response.Body, body, 20)
	if err != nil {
		err = fmt.Errorf("error reading response for %s: %w", fullURL, err)
		plugin.log(LogLevelError, err.Error())
		plugin.completeRequest(token, err)
		return err
	}

	result := string(body)
	if strings.HasPrefix(result, "BEGIN") {
		plugin.setCache(token)
		plugin.log(LogLevelDebug, "Valid response received for token: %s", token)
		plugin.completeRequest(token, nil)
		return nil
	} else {
		err = fmt.Errorf("invalid response content for token: %s", token)
		plugin.log(LogLevelDebug, "Invalid response for token: %s. Response: %s", token, result)
		plugin.completeRequest(token, err)
		return err
	}
}

// completeRequest marks a request as complete and notifies all waiting goroutines
func (plugin *ICalMiddleware) completeRequest(token string, err error) {
	plugin.inFlightRequestMutex.Lock()
	defer plugin.inFlightRequestMutex.Unlock()
	
	if multiplexer, exists := plugin.inFlightRequests[token]; exists {
		// Store the result
		multiplexer.err = err
		multiplexer.isRunning = false
		
		// Notify all waiting goroutines
		close(multiplexer.done)
		
		// Clean up immediately if successful to save memory
		if err == nil {
			delete(plugin.inFlightRequests, token)
		} else {
			// For errors, keep the result around briefly to avoid hammering the upstream
			go func() {
				time.Sleep(30 * time.Second)
				plugin.inFlightRequestMutex.Lock()
				defer plugin.inFlightRequestMutex.Unlock()
				
				// Only delete if it's the same instance and not running
				if m, exists := plugin.inFlightRequests[token]; exists && m == multiplexer && !m.isRunning {
					delete(plugin.inFlightRequests, token)
				}
			}()
		}
	}
}

// cleanupStaleRequests removes old in-flight requests and rate limiters
func (plugin *ICalMiddleware) cleanupStaleRequests() {
	now := time.Now()
	
	// Only run cleanup at the configured interval
	if now.Sub(plugin.lastCleanup) < plugin.cleanupInterval {
		return
	}
	plugin.lastCleanup = now
	
	// Cleanup in-flight requests
	plugin.inFlightRequestMutex.Lock()
	for token, multiplexer := range plugin.inFlightRequests {
		if !multiplexer.isRunning && now.Sub(multiplexer.timestamp) > 5*time.Minute {
			delete(plugin.inFlightRequests, token)
		}
	}
	plugin.inFlightRequestMutex.Unlock()
	
	// Cleanup IP rate limiters
	plugin.ipLimiterMutex.Lock()
	// Consider IPs stale after 1 hour of inactivity
	staleTime := now.Add(-1 * time.Hour)
	for ip := range plugin.ipLimiters {
		// This is a simple approach - in a real implementation, we would track last use time
		if len(plugin.ipLimiters) > 1000 {
			delete(plugin.ipLimiters, ip)
		}
	}
	plugin.ipLimiterMutex.Unlock()
}

// extractTokenFromHeader extracts the token from the request header
func (plugin *ICalMiddleware) extractTokenFromHeader(request *http.Request) string {
	canonicalHeaderName := http.CanonicalHeaderKey(plugin.headerName)
	token := request.Header.Get(canonicalHeaderName)
	if token == "" {
		return ""
	}

	if len(token) > 7 && strings.EqualFold(token[:7], "Bearer ") {
		token = token[7:]
	}

	if !plugin.forwardToken {
		request.Header.Del(canonicalHeaderName)
	}

	return token
}

// ReadUserIP extracts the user's IP address from the request
func ReadUserIP(r *http.Request) string {
	// Try X-Real-IP first (common in proxy setups)
	ipAddress := r.Header.Get("X-Real-Ip")
	if ipAddress == "" {
		// Try X-Forwarded-For (standard proxy header)
		ipAddress = r.Header.Get("X-Forwarded-For")
		if ipAddress != "" {
			// X-Forwarded-For can contain multiple IPs, take the first one
			parts := strings.Split(ipAddress, ",")
			if len(parts) > 0 {
				ipAddress = strings.TrimSpace(parts[0])
			}
		}
	}
	// Fall back to RemoteAddr if no proxy headers are present
	if ipAddress == "" {
		ipAddress, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	return ipAddress
}

// containsSubnet checks if an IP address is in the allowed subnets
func (plugin *ICalMiddleware) containsSubnet(address string) bool {
	ip, err := netip.ParseAddr(address)
	if err != nil {
		plugin.log(LogLevelError, "Invalid IP '%s': %v", address, err)
		return false
	}

	for _, prefix := range plugin.allowSubnet {
		if prefix.Contains(ip) {
			plugin.log(LogLevelDebug, "IP %v is in subnet %v", ip, prefix)
			return true
		}
	}
	plugin.log(LogLevelDebug, "IP %v not found in allowed subnets", ip)
	return false
}

// checkRateLimit checks if a request exceeds the rate limits
func (plugin *ICalMiddleware) checkRateLimit(userIP string) (int, error) {
	// Check global rate limit
	if plugin.globalLimiter != nil && !plugin.globalLimiter.Allow() {
		plugin.log(LogLevelError, "Global rate limit exceeded")
		return http.StatusTooManyRequests, fmt.Errorf("global rate limit exceeded")
	}

	// Check per-IP rate limit
	if plugin.perIPRateLimit > 0 && plugin.rateLimitWindow > 0 {
		// First try with a read lock
		plugin.ipLimiterMutex.RLock()
		limiter, exists := plugin.ipLimiters[userIP]
		plugin.ipLimiterMutex.RUnlock()
		
		if !exists {
			// If not found, acquire write lock and create
			plugin.ipLimiterMutex.Lock()
			limiter, exists = plugin.ipLimiters[userIP]
			if !exists {
				ratePerSec := float64(plugin.perIPRateLimit) / plugin.rateLimitWindow.Seconds()
				limiter = rate.NewLimiter(rate.Limit(ratePerSec), plugin.perIPRateLimit)
				plugin.ipLimiters[userIP] = limiter
			}
			plugin.ipLimiterMutex.Unlock()
		}
		
		if !limiter.Allow() {
			plugin.log(LogLevelError, "Rate limit exceeded for IP: %s", userIP)
			return http.StatusTooManyRequests, fmt.Errorf("rate limit exceeded for IP %s", userIP)
		}
	}
	
	return http.StatusOK, nil
}

// validate validates a request
func (plugin *ICalMiddleware) validate(request *http.Request) (int, error) {
	userIP := ReadUserIP(request)
	plugin.log(LogLevelDebug, "Processing request from IP: %s, URL: %s", userIP, request.URL.String())

	// Run cleanup periodically
	plugin.cleanupStaleRequests()

	// Check if IP is in allowed subnets
	if plugin.containsSubnet(userIP) {
		plugin.log(LogLevelDebug, "IP %s is in allowed subnet", userIP)
		return http.StatusOK, nil
	}

	// Check rate limits
	if status, err := plugin.checkRateLimit(userIP); err != nil {
		return status, err
	}

	// Extract and validate token
	token := plugin.extractTokenFromHeader(request)

	// Check if token is in cache
	if token != "" {
		// Check cache first (optimization)
		if plugin.cache.Has(token) {
			plugin.log(LogLevelDebug, "Token '%s' found in cache for IP %s", token, userIP)
			return http.StatusOK, nil
		}
		
		// Validate token length
		if len(token) != 16 {
			plugin.log(LogLevelError, "Incorrect token length '%s' for request from IP %s", token, userIP)
			return http.StatusUnauthorized, fmt.Errorf("incorrect token length")
		}
		
		// Validate token with upstream
		err := plugin.httpRequestAndCache(token)
		if err != nil {
			plugin.log(LogLevelError, "Token validation failed for '%s' from IP %s: %v", token, userIP, err)
			return http.StatusUnauthorized, err
		}
		
		plugin.log(LogLevelInfo, "Token '%s' validated and cached for IP %s", token, userIP)
		return http.StatusOK, nil
	}
	
	plugin.log(LogLevelError, "No token provided in header '%s' for request from IP %s", plugin.headerName, userIP)
	return http.StatusUnauthorized, fmt.Errorf("no token provided")
}

// ServeHTTP handles the HTTP request
func (plugin *ICalMiddleware) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	// Handle OPTIONS requests for CORS
	if req.Method == http.MethodOptions {
		origin := req.Header.Get("Origin")
		if origin != "" {
			rw.Header().Set("Access-Control-Allow-Origin", origin)
			rw.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			rw.Header().Set("Access-Control-Allow-Headers", plugin.headerName+", Content-Type")
			rw.Header().Set("Access-Control-Max-Age", "86400") // 24 hours
			rw.WriteHeader(http.StatusOK)
			return
		}
	}

	// Validate the request
	status, err := plugin.validate(req)
	if err != nil {
		origin := req.Header.Get("Origin")
		if origin != "" {
			rw.Header().Set("Cache-Control", "no-cache")
			rw.Header().Set("Access-Control-Allow-Origin", origin)
			rw.Header().Set("Access-Control-Allow-Headers", "*")
			rw.Header().Set("Access-Control-Max-Age", "86400") // 24 hours
		}
		
		// Add standard security headers
		rw.Header().Set("X-Content-Type-Options", "nosniff")
		rw.Header().Set("X-Frame-Options", "DENY")
		
		http.Error(rw, fmt.Sprintf("Unauthorized. Attach valid ICal ETIS token in '%s' header", plugin.headerName), status)
		return
	}
	
	// Add tracing header to indicate the request was processed by this middleware
	req.Header.Set("X-Processed-By", "ical-middleware")
	
	// Pass the request to the next handler
	plugin.next.ServeHTTP(rw, req)
}
