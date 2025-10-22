package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/eiannone/keyboard"
	"github.com/joho/godotenv"
	hook "github.com/robotn/gohook"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/spotify"
)

// Configuration loaded from environment
var (
	clientID     string
	clientSecret string
	localPort    string
	certFile     string
	keyFile      string
	redirectURL  string
	tokenFile    = "spotify_token.json"
)

// Keyboard shortcuts configuration - loaded from env
var shortcuts map[rune]ShortcutAction

type ShortcutAction struct {
	Name   string
	Action func(*http.Client) error
}

var oauthConfig *oauth2.Config

func init() {
	// Load .env file if it exists (won't error if file doesn't exist)
	godotenv.Load()

	// Load configuration from environment
	clientID = getEnv("EZSPOTIFY_CLIENT_ID", "")
	clientSecret = getEnv("EZSPOTIFY_CLIENT_SECRET", "")
	localPort = getEnv("EZSPOTIFY_LOCAL_PORT", "9120")
	certFile = getEnv("EZSPOTIFY_CERT_FILE", "cert.pem")
	keyFile = getEnv("EZSPOTIFY_KEY_FILE", "key.pem")
	redirectURL = "https://127.0.0.1:" + localPort + "/callback"

	if clientID == "" || clientSecret == "" {
		log.Fatal("EZSPOTIFY_CLIENT_ID and EZSPOTIFY_CLIENT_SECRET must be set")
	}

	// Initialize OAuth config
	oauthConfig = &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Scopes: []string{
			"user-modify-playback-state",
			"user-read-playback-state",
		},
		Endpoint: spotify.Endpoint,
	}

	// Load keyboard shortcuts from environment
	shortcuts = map[rune]ShortcutAction{
		rune(getEnv("EZSPOTIFY_KEY_PLAY_PAUSE", " ")[0]): {Name: "Play/Pause", Action: togglePlayback},
		rune(getEnv("EZSPOTIFY_KEY_NEXT", "n")[0]):       {Name: "Next Track", Action: nextTrack},
		rune(getEnv("EZSPOTIFY_KEY_PREV", "p")[0]):       {Name: "Previous Track", Action: previousTrack},
		rune(getEnv("EZSPOTIFY_KEY_VOLUME_UP", "+")[0]):  {Name: "Volume Up", Action: volumeUp},
		rune(getEnv("EZSPOTIFY_KEY_VOLUME_DOWN", "-")[0]): {Name: "Volume Down", Action: volumeDown},
		rune(getEnv("EZSPOTIFY_KEY_MUTE", "m")[0]):       {Name: "Mute", Action: mute},
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func main() {
	token, err := loadToken()
	if err != nil {
		log.Println("No valid token found, starting OAuth flow...")
		token, err = authenticate()
		if err != nil {
			log.Fatal("Authentication failed:", err)
		}
	}

	client := createAutoRefreshClient(token)

	fmt.Println("\n🎵 Spotify Controller Ready!")
	fmt.Println("Available shortcuts:")
	for key, shortcut := range shortcuts {
		if key == ' ' {
			fmt.Printf("  [Space] - %s\n", shortcut.Name)
		} else {
			fmt.Printf("  [%c] - %s\n", key, shortcut.Name)
		}
	}
	fmt.Println("  [q] - Quit")
	fmt.Println("  Media keys (Play/Pause, Next, Previous) are also supported")
	fmt.Println()

	// Start media key listener in background
	go listenMediaKeys(client)

	if err := keyboard.Open(); err != nil {
		log.Fatal("Failed to initialize keyboard:", err)
	}
	defer keyboard.Close()

	for {
		char, key, err := keyboard.GetKey()
		if err != nil {
			log.Println("Error reading key:", err)
			continue
		}

		if key == keyboard.KeyEsc || char == 'q' {
			fmt.Println("\nExiting...")
			break
		}

		if shortcut, exists := shortcuts[char]; exists {
			fmt.Printf("Executing: %s\n", shortcut.Name)
			if err := shortcut.Action(client); err != nil {
				log.Printf("Error executing %s: %v\n", shortcut.Name, err)
			}
		}
	}
}

func listenMediaKeys(client *http.Client) {
	evChan := hook.Start()
	defer hook.End()

	for ev := range evChan {
		if ev.Kind != hook.KeyDown {
			continue
		}

		var action func(*http.Client) error
		var actionName string

		switch ev.Rawcode {
		case 179: // Play/Pause (Windows/Linux)
			action = togglePlayback
			actionName = "Play/Pause"
		case 176: // Next track (Windows/Linux)
			action = nextTrack
			actionName = "Next Track"
		case 177: // Previous track (Windows/Linux)
			action = previousTrack
			actionName = "Previous Track"
		}

		if action != nil {
			fmt.Printf("Media key: %s\n", actionName)
			if err := action(client); err != nil {
				log.Printf("Error executing %s: %v\n", actionName, err)
			}
		}
	}
}

func authenticate() (*oauth2.Token, error) {
	state := "random-state-string"
	authURL := oauthConfig.AuthCodeURL(state, oauth2.AccessTypeOffline)

	codeChan := make(chan string)
	errChan := make(chan error)

	server := &http.Server{Addr: "127.0.0.1:" + localPort}
	http.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			errChan <- fmt.Errorf("state mismatch")
			return
		}

		code := r.URL.Query().Get("code")
		if code == "" {
			errChan <- fmt.Errorf("no code in response")
			return
		}

		fmt.Fprintf(w, "Authorization successful! You can close this window.")
		codeChan <- code
	})

	go server.ListenAndServeTLS(certFile, keyFile)
	defer server.Shutdown(context.Background())

	fmt.Println("Opening browser for authorization...")
	fmt.Println("If browser doesn't open, visit this URL:")
	fmt.Println(authURL)

	var code string
	select {
	case code = <-codeChan:
	case err := <-errChan:
		return nil, err
	case <-time.After(5 * time.Minute):
		return nil, fmt.Errorf("authorization timeout")
	}

	token, err := oauthConfig.Exchange(context.Background(), code)
	if err != nil {
		return nil, err
	}

	saveToken(token)
	return token, nil
}

func createAutoRefreshClient(token *oauth2.Token) *http.Client {
	tokenSource := oauthConfig.TokenSource(context.Background(), token)
	
	// Wrap token source to save refreshed tokens
	wrappedSource := &autoSaveTokenSource{
		src: tokenSource,
	}
	
	return oauth2.NewClient(context.Background(), wrappedSource)
}

type autoSaveTokenSource struct {
	src oauth2.TokenSource
}

func (a *autoSaveTokenSource) Token() (*oauth2.Token, error) {
	token, err := a.src.Token()
	if err != nil {
		return nil, err
	}
	saveToken(token)
	return token, nil
}

func saveToken(token *oauth2.Token) error {
	data, err := json.Marshal(token)
	if err != nil {
		return err
	}
	return os.WriteFile(tokenFile, data, 0600)
}

func loadToken() (*oauth2.Token, error) {
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, err
	}
	var token oauth2.Token
	if err := json.Unmarshal(data, &token); err != nil {
		return nil, err
	}
	return &token, nil
}

// Spotify API Actions
func togglePlayback(client *http.Client) error {
	resp, err := client.Get("https://api.spotify.com/v1/me/player")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 204 {
		return fmt.Errorf("no active device")
	}

	var state struct {
		IsPlaying bool `json:"is_playing"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return err
	}

	endpoint := "https://api.spotify.com/v1/me/player/pause"
	if !state.IsPlaying {
		endpoint = "https://api.spotify.com/v1/me/player/play"
	}

	req, _ := http.NewRequest("PUT", endpoint, nil)
	resp, err = client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func nextTrack(client *http.Client) error {
	req, _ := http.NewRequest("POST", "https://api.spotify.com/v1/me/player/next", nil)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func previousTrack(client *http.Client) error {
	req, _ := http.NewRequest("POST", "https://api.spotify.com/v1/me/player/previous", nil)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func volumeUp(client *http.Client) error {
	return adjustVolume(client, 10)
}

func volumeDown(client *http.Client) error {
	return adjustVolume(client, -10)
}

func mute(client *http.Client) error {
	req, _ := http.NewRequest("PUT", "https://api.spotify.com/v1/me/player/volume?volume_percent=0", nil)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func adjustVolume(client *http.Client, delta int) error {
	resp, err := client.Get("https://api.spotify.com/v1/me/player")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var state struct {
		Device struct {
			VolumePercent int `json:"volume_percent"`
		} `json:"device"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return err
	}

	newVolume := state.Device.VolumePercent + delta
	if newVolume < 0 {
		newVolume = 0
	}
	if newVolume > 100 {
		newVolume = 100
	}

	req, _ := http.NewRequest("PUT", fmt.Sprintf("https://api.spotify.com/v1/me/player/volume?volume_percent=%d", newVolume), nil)
	resp, err = client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
