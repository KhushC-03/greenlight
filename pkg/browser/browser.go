package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/SecretBots/greenlight/pkg/page"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type ProxyConfig struct {
	Host     string // proxy hostname or IP
	Port     int    // proxy port
	Username string // optional username
	Password string // optional password
	Protocol string // http or socks5
}

type Browser struct {
	execPath     string
	wsEndpoint   string
	conn         *websocket.Conn
	cmd          *exec.Cmd
	context      context.Context
	cancel       context.CancelFunc
	userDataDir  string
	messageID    int
	messageMutex sync.Mutex
	pid          int
	isHeadless   bool
	cookies      []Cookie
	proxy        *ProxyConfig // added for proxy support
}

type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	Session  bool    `json:"session"`
	SameSite string  `json:"sameSite"`
}

func GreenLight(execPath string, isHeadless bool, startURL string, sessionDir string, proxy *ProxyConfig) *Browser {
	ctx, cancel := context.WithCancel(context.Background())

	var userDataDir string
	if sessionDir != "" {
		userDataDir = sessionDir
	} else {
		userDataDir = filepath.Join(os.TempDir(), fmt.Sprintf("greenlight_%s", uuid.New().String()))
	}

	browser := &Browser{
		execPath:    execPath,
		context:     ctx,
		cancel:      cancel,
		userDataDir: userDataDir,
		isHeadless:  isHeadless,
		cookies:     make([]Cookie, 0),
		proxy:       proxy,
	}

	if err := browser.launch(startURL); err != nil {
		log.Fatalf("Failed to launch browser: %v", err)
	}

	// Load cookies after launch
	if cookies, err := browser.GetCookies(); err == nil {
		browser.cookies = cookies
	}

	return browser
}

func (b *Browser) launch(startURL string) error {
	debugPort := "9222"
	args := []string{
		"--remote-debugging-port=" + debugPort,
		"--no-first-run",
		"--user-data-dir=" + b.userDataDir,
		"--remote-allow-origins=*",
		startURL,
	}

	if b.isHeadless {
		args = append(args, "--headless=new")
	}

	// Apply proxy configuration if provided
	if b.proxy != nil {
		protocol := b.proxy.Protocol
		if protocol == "" {
			protocol = "http"
		}
		proxyArg := fmt.Sprintf("--proxy-server=%s://%s:%d", protocol, b.proxy.Host, b.proxy.Port)
		args = append(args, proxyArg)

		log.Printf("Using proxy: %s", proxyArg)
	}

	b.cmd = exec.CommandContext(b.context, b.execPath, args...)
	if err := b.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start browser: %v", err)
	}

	b.pid = b.cmd.Process.Pid
	log.Printf("Chrome started with PID: %d", b.pid)

	// Allow Chrome to start
	time.Sleep(1 * time.Second)

	if err := b.attachToPage(); err != nil {
		return err
	}

	// Inject proxy authentication headers if needed
	if b.proxy != nil && b.proxy.Username != "" {
		if err := b.injectProxyAuthHeader(); err != nil {
			log.Printf("Warning: Failed to inject proxy auth header: %v", err)
		}
	}

	return nil
}

func (b *Browser) injectProxyAuthHeader() error {
	if b.conn == nil {
		return fmt.Errorf("WebSocket connection not established")
	}

	authHeader := "Basic " + base64.StdEncoding.EncodeToString(
		[]byte(fmt.Sprintf("%s:%s", b.proxy.Username, b.proxy.Password)),
	)

	params := map[string]interface{}{
		"headers": map[string]interface{}{
			"Proxy-Authorization": authHeader,
		},
	}

	_, err := b.SendCommandWithResponse("Network.setExtraHTTPHeaders", params)
	if err != nil {
		return fmt.Errorf("failed to set proxy authorization header: %v", err)
	}
	log.Println("Proxy authentication header injected successfully.")
	return nil
}

func (b *Browser) attachToPage() error {
	debugPort := "9222"
	resp, err := http.Get(fmt.Sprintf("http://localhost:%s/json", debugPort))
	if err != nil {
		return fmt.Errorf("failed to fetch active pages: %v", err)
	}
	defer resp.Body.Close()

	var pages []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&pages); err != nil {
		return fmt.Errorf("failed to decode JSON: %v", err)
	}

	for _, page := range pages {
		if page["type"] == "page" && page["url"] != "" {
			if wsURL, ok := page["webSocketDebuggerUrl"].(string); ok {
				if b.conn != nil {
					b.conn.Close()
				}
				conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
				if err != nil {
					return fmt.Errorf("failed to connect to page WebSocket: %v", err)
				}
				b.conn = conn
				b.wsEndpoint = wsURL
				log.Printf("Connected to page: %s", page["url"])
				return nil
			}
		}
	}
	return fmt.Errorf("no suitable page found")
}
func (b *Browser) SendCommandWithResponse(method string, params map[string]interface{}) (map[string]interface{}, error) {
	b.messageMutex.Lock()
	b.messageID++
	id := b.messageID
	b.messageMutex.Unlock()

	message := map[string]interface{}{
		"id":     id,
		"method": method,
		"params": params,
	}

	if b.conn == nil {
		if err := b.attachToPage(); err != nil {
			return nil, fmt.Errorf("failed to reconnect WebSocket: %v", err)
		}
	}

	if err := b.conn.WriteJSON(message); err != nil {
		return nil, fmt.Errorf("failed to send WebSocket message: %v", err)
	}

	for {
		_, data, err := b.conn.ReadMessage()
		if err != nil {
			return nil, fmt.Errorf("failed to read WebSocket message: %v", err)
		}

		var response map[string]interface{}
		if err := json.Unmarshal(data, &response); err != nil {
			log.Printf("Failed to parse WebSocket message: %s", string(data))
			continue
		}

		if responseID, ok := response["id"].(float64); ok && int(responseID) == id {
			return response, nil
		}
	}
}

func (b *Browser) SendCommandWithoutResponse(method string, params map[string]interface{}) error {
	b.messageMutex.Lock()
	b.messageID++
	id := b.messageID
	b.messageMutex.Unlock()

	message := map[string]interface{}{
		"id":     id,
		"method": method,
		"params": params,
	}

	if b.conn == nil {
		if err := b.attachToPage(); err != nil {
			return fmt.Errorf("failed to reconnect WebSocket: %v", err)
		}
	}

	if err := b.conn.WriteJSON(message); err != nil {
		return fmt.Errorf("failed to send WebSocket message: %v", err)
	}

	return nil
}

func (b *Browser) NewPage() *page.Page {
	if b.conn == nil {
		log.Fatal("WebSocket connection not established. Cannot create a new page.")
	}
	return page.NewPage(b)
}

func (b *Browser) RedLight() {
	if b.conn != nil {
		if err := b.conn.Close(); err != nil {
			log.Printf("Error closing WebSocket: %v", err)
		}
	}

	if b.cmd != nil && b.cmd.Process != nil {
		if err := b.cmd.Process.Kill(); err != nil {
			log.Printf("Error killing browser process: %v", err)
		} else {
			b.cmd.Wait()
		}
	}

	if b.userDataDir != "" {
		if err := os.RemoveAll(b.userDataDir); err != nil {
			log.Printf("Error removing user data directory: %v", err)
		}
	}

	b.cancel()
	log.Println("Browser closed successfully.")
}

func (b *Browser) GetCookies() ([]Cookie, error) {
	params := map[string]interface{}{
		"urls": []string{}, // Empty array means all cookies
	}

	response, err := b.SendCommandWithResponse("Network.getAllCookies", params)
	if err != nil {
		return nil, fmt.Errorf("failed to get cookies: %v", err)
	}

	if result, ok := response["result"].(map[string]interface{}); ok {
		if cookiesRaw, ok := result["cookies"].([]interface{}); ok {
			cookiesBytes, err := json.Marshal(cookiesRaw)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal cookies: %v", err)
			}

			var cookies []Cookie
			if err := json.Unmarshal(cookiesBytes, &cookies); err != nil {
				return nil, fmt.Errorf("failed to unmarshal cookies: %v", err)
			}

			b.cookies = cookies

			return cookies, nil
		}
	}

	return nil, fmt.Errorf("unexpected response format")
}

func (b *Browser) SetCookie(cookie Cookie) error {
	params := map[string]interface{}{
		"name":     cookie.Name,
		"value":    cookie.Value,
		"domain":   cookie.Domain,
		"path":     cookie.Path,
		"expires":  cookie.Expires,
		"httpOnly": cookie.HTTPOnly,
		"secure":   cookie.Secure,
		"sameSite": cookie.SameSite,
	}

	_, err := b.SendCommandWithResponse("Network.setCookie", params)
	if err != nil {
		return fmt.Errorf("failed to set cookie: %v", err)
	}

	// Update local storage after successful cookie setting
	found := false
	for i, existingCookie := range b.cookies {
		if existingCookie.Name == cookie.Name && existingCookie.Domain == cookie.Domain {
			b.cookies[i] = cookie
			found = true
			break
		}
	}

	if !found {
		b.cookies = append(b.cookies, cookie)
	}

	return nil
}

func (b *Browser) DeleteCookies(name, domain string) error {
	params := map[string]interface{}{
		"name":   name,
		"domain": domain,
	}

	_, err := b.SendCommandWithResponse("Network.deleteCookies", params)
	if err != nil {
		return fmt.Errorf("failed to delete cookies: %v", err)
	}

	// Update local storage by removing matching cookies
	var filteredCookies []Cookie
	for _, cookie := range b.cookies {
		if !(cookie.Name == name && cookie.Domain == domain) {
			filteredCookies = append(filteredCookies, cookie)
		}
	}
	b.cookies = filteredCookies

	return nil
}

// Find a specific cookie by name and domain
func (b *Browser) FindCookie(name, domain string) (Cookie, bool) {
	for _, cookie := range b.cookies {
		if cookie.Name == name && cookie.Domain == domain {
			return cookie, true
		}
	}
	return Cookie{}, false
}

// Refresh all cookies from the browser
func (b *Browser) RefreshCookies() error {
	cookies, err := b.GetCookies()
	if err != nil {
		return err
	}
	b.cookies = cookies
	return nil
}
