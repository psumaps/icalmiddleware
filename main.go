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

type Config struct {
	ForwardToken    bool     `json:"forwardToken,omitempty"`
	Freshness       int64    `json:"freshness,omitempty"`
	HeaderName      string   `json:"headerName,omitempty"`
	AllowSubnet     []string `json:"allowSubnet,omitempty"`
	Timeout         int64    `json:"timeout,omitempty"`
	GlobalRateLimit int      `json:"globalRateLimit,omitempty"`
	PerIPRateLimit  int      `json:"perIPRateLimit,omitempty"`
	RateLimitWindow int64    `json:"rateLimitWindow,omitempty"`
}

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
	}
}

type ICalMiddleware struct {
	next         http.Handler
	headerName   string
	forwardToken bool
	freshness    int64
	cache        *Cache
	allowSubnet  []netip.Prefix
	timeout      time.Duration
	name         string

	// Поля для rate limiting
	globalLimiter   *rate.Limiter
	ipLimiters      map[string]*rate.Limiter
	ipLimiterMutex  sync.Mutex
	perIPRateLimit  int
	rateLimitWindow time.Duration
}

func New(_ context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	var cidrs []netip.Prefix
	for _, cidr := range config.AllowSubnet {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			fmt.Printf("[ERROR] [%s] Неверная подсеть '%s': %v\n", name, cidr, err)
			continue
		}
		cidrs = append(cidrs, prefix)
	}
	if len(cidrs) == 0 {
		return nil, fmt.Errorf("не предоставлено ни одной валидной подсети")
	}

	timeout := time.Duration(config.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	cache := NewCache(time.Duration(config.Freshness)*time.Second, 8*time.Hour)

	var globalLimiter *rate.Limiter
	if config.GlobalRateLimit > 0 && config.RateLimitWindow > 0 {
		ratePerSec := float64(config.GlobalRateLimit) / float64(config.RateLimitWindow)
		globalLimiter = rate.NewLimiter(rate.Limit(ratePerSec), config.GlobalRateLimit)
	}

	return &ICalMiddleware{
		headerName:      config.HeaderName,
		forwardToken:    config.ForwardToken,
		freshness:       config.Freshness,
		allowSubnet:     cidrs,
		next:            next,
		cache:           cache,
		timeout:         timeout,
		name:            name,
		globalLimiter:   globalLimiter,
		ipLimiters:      make(map[string]*rate.Limiter),
		perIPRateLimit:  config.PerIPRateLimit,
		rateLimitWindow: time.Duration(config.RateLimitWindow) * time.Second,
	}, nil
}

func (plugin *ICalMiddleware) setCache(key string) {
	plugin.cache.Set(key, true, 0)
}

func (plugin *ICalMiddleware) httpRequestAndCache(url string) error {
	fullURL := "https://ical.psu.ru/calendars/" + url
	ctx, cancel := context.WithTimeout(context.Background(), plugin.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return fmt.Errorf("[ERROR] [%s] Ошибка создания запроса для %s: %v", plugin.name, fullURL, err)
	}

	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("[ERROR] [%s] Ошибка запроса для %s: %v", plugin.name, fullURL, err)
	}
	defer response.Body.Close()

	body := make([]byte, 20)
	_, err = io.ReadAtLeast(response.Body, body, 20)
	if err != nil {
		return fmt.Errorf("[ERROR] [%s] Ошибка чтения ответа для %s: %v", plugin.name, fullURL, err)
	}

	result := string(body)
	if strings.HasPrefix(result, "BEGIN") {
		plugin.setCache(url)
		fmt.Printf("[DEBUG] [%s] Валидный ответ получен для токена: %s\n", plugin.name, url)
	} else {
		fmt.Printf("[DEBUG] [%s] Невалидный ответ для токена: %s. Ответ: %s\n", plugin.name, url, result)
		return fmt.Errorf("[ERROR] [%s] Запрос невалиден для токена: %s", plugin.name, url)
	}

	return nil
}

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

func ReadUserIP(r *http.Request) string {
	ipAddress := r.Header.Get("X-Real-Ip")
	if ipAddress == "" {
		ipAddress = r.Header.Get("X-Forwarded-For")
	}
	if ipAddress == "" {
		ipAddress, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	return ipAddress
}

func (plugin *ICalMiddleware) containsSubnet(address string) bool {
	ip, err := netip.ParseAddr(address)
	if err != nil {
		fmt.Printf("[ERROR] [%s] Неверный IP '%s': %v\n", plugin.name, address, err)
		return false
	}

	for _, prefix := range plugin.allowSubnet {
		if prefix.Contains(ip) {
			fmt.Printf("[DEBUG] [%s] IP %v входит в подсеть %v\n", plugin.name, ip, prefix)
			return true
		}
	}
	fmt.Printf("[DEBUG] [%s] IP %v не найден в разрешённых подсетях\n", plugin.name, ip)
	return false
}

func (plugin *ICalMiddleware) validate(request *http.Request) (int, error) {
	userIP := ReadUserIP(request)
	fmt.Printf("[DEBUG] [%s] Обработка запроса от IP: %s, URL: %s\n", plugin.name, userIP, request.URL.String())

	if plugin.containsSubnet(userIP) {
		fmt.Printf("[DEBUG] [%s] IP %s входит в разрешённую подсет\n", plugin.name, userIP)
		return http.StatusOK, nil
	}

	if plugin.globalLimiter != nil && !plugin.globalLimiter.Allow() {
		fmt.Printf("[ERROR] [%s] Превышен глобальный лимит запросов\n", plugin.name)
		return http.StatusTooManyRequests, fmt.Errorf("global rate limit exceeded")
	}

	if plugin.perIPRateLimit > 0 && plugin.rateLimitWindow > 0 {
		plugin.ipLimiterMutex.Lock()
		limiter, exists := plugin.ipLimiters[userIP]
		if !exists {
			ratePerSec := float64(plugin.perIPRateLimit) / plugin.rateLimitWindow.Seconds()
			limiter = rate.NewLimiter(rate.Limit(ratePerSec), plugin.perIPRateLimit)
			plugin.ipLimiters[userIP] = limiter
		}
		plugin.ipLimiterMutex.Unlock()
		if !limiter.Allow() {
			fmt.Printf("[ERROR] [%s] Превышен лимит запросов для IP: %s\n", plugin.name, userIP)
			return http.StatusTooManyRequests, fmt.Errorf("rate limit exceeded for IP %s", userIP)
		}
	}

	token := plugin.extractTokenFromHeader(request)
	if token == "" {
		fmt.Printf("[ERROR] [%s] Токен не предоставлен в заголовке '%s' для запроса от IP %s\n", plugin.name, plugin.headerName, userIP)
		return http.StatusUnauthorized, fmt.Errorf("no token provided")
	}
	if len(token) != 16 {
		fmt.Printf("[ERROR] [%s] Неверная длина токена '%s' для запроса от IP %s\n", plugin.name, token, userIP)
		return http.StatusUnauthorized, fmt.Errorf("incorrect token len")
	}
	if !plugin.cache.Has(token) {
		err := plugin.httpRequestAndCache(token)
		if err != nil {
			fmt.Printf("[ERROR] [%s] Проверка токена '%s' не пройдена для запроса от IP %s: %v\n", plugin.name, token, userIP, err)
			return http.StatusUnauthorized, err
		}
		fmt.Printf("[DEBUG] [%s] Токен '%s' валидирован и кэширован для IP %s\n", plugin.name, token, userIP)
	} else {
		fmt.Printf("[DEBUG] [%s] Токен '%s' найден в кэше для IP %s\n", plugin.name, token, userIP)
	}

	return http.StatusOK, nil
}

func (plugin *ICalMiddleware) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	status, err := plugin.validate(req)
	if err != nil {
		origin := req.Header.Get("Origin")
		if origin != "" {
			rw.Header().Add("Cache-Control", "no-cache")
			rw.Header().Add("Access-Control-Allow-Origin", origin)
			rw.Header().Add("Access-Control-Allow-Headers", "*")
			rw.Header().Add("Access-Control-Max-Age", "0")
		}
		http.Error(rw, fmt.Sprintf("Unauthorized. Attach valid ICal ETIS token in '%s' header", plugin.headerName), status)
		return
	}
	plugin.next.ServeHTTP(rw, req)
}
