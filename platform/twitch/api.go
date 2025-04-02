package twitch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	helixURL = "https://api.twitch.tv/helix"
	gqlURL   = "https://gql.twitch.tv/gql"

	// Refresh the token 5 minutes before expiration.
	appTokenBufferTime = 5 * time.Minute
)

// Twitch API APIClient.
// Automatically gets app access OAuth tokens and refreshes when needed.
// clientID, clientSecret, and userToken must be provided to the client upon creation.
type APIClient struct {
	clientID     string
	clientSecret string
	httpClient   *http.Client

	// User access token for GQL queries from device code grant flow or web frontend
	// https://dev.twitch.tv/docs/authentication/getting-tokens-oauth/#device-code-grant-flow
	userToken      string
	userTokenMutex sync.RWMutex

	// App access token for twitch helix api from client credentials grant flow
	// https://dev.twitch.tv/docs/authentication/getting-tokens-oauth/#client-credentials-grant-flow
	appToken                    string
	appTokenExpiry              time.Time
	appTokenNextValidationCheck time.Time
	appTokenMutex               sync.RWMutex
}

type APIError struct {
	Status  int
	Message string
}

func (e APIError) Error() string {
	return fmt.Sprintf("twitch API error (status %d): %s", e.Status, e.Message)
}

type GetStreamsParams struct {
	UserIDs    []string
	UserLogins []string
}

// /helix/streams
type APIGetStreamsResponse struct {
	Data []struct {
		UserID      string    `json:"user_id"`
		UserLogin   string    `json:"user_login"`
		GameName    string    `json:"game_name"`
		ViewerCount int       `json:"viewer_count"`
		StartedAt   time.Time `json:"started_at"`
	} `json:"data"`
}

type GetUsersParams GetStreamsParams

type APIGetUsersResponse struct {
	Data         []APIGetUsersData `json:"data"`
	userIdMap    map[string]*APIGetUsersData
	userLoginMap map[string]*APIGetUsersData
}

type APIGetUsersData struct {
	UserID      string `json:"id"`
	UserLogin   string `json:"login"`
	DisplayName string `json:"display_name"`
}

func (r *APIGetUsersResponse) GetUserByID(id string) (*APIGetUsersData, bool) {
	if id == "" {
		return nil, false
	}
	user, exists := r.userIdMap[id]
	return user, exists
}

func (r *APIGetUsersResponse) GetUserByLogin(login string) (*APIGetUsersData, bool) {
	if login == "" {
		return nil, false
	}
	user, exists := r.userLoginMap[strings.ToLower(login)]
	return user, exists
}

// Twitch GQL response for playbackaccesstoken request endpoint
type GQLPlaybackAccessTokenResponse struct {
	Data struct {
		Token StreamPlaybackAccessToken `json:"streamPlaybackAccessToken"`
	} `json:"data"`
}

type StreamPlaybackAccessToken struct {
	Value     string `json:"value"`
	Signature string `json:"signature"`
}

func NewAPIClient(clientID string, clientSecret string, userToken string, httpClient *http.Client) *APIClient {
	return &APIClient{
		clientID:     clientID,
		clientSecret: clientSecret,
		userToken:    userToken,
		httpClient:   httpClient,
	}
}

func (c *APIClient) ReplaceUserToken(userToken string) {
	c.userTokenMutex.Lock()
	c.userToken = userToken
	c.userTokenMutex.Unlock()
}

func (c *APIClient) ensureValidUserToken() error {
	c.userTokenMutex.RLock()
	defer c.userTokenMutex.RUnlock()

	err := c.validateOAuthToken(c.userToken)
	if err != nil {
		return fmt.Errorf("failed to validate user token: %v", err)
	}

	return nil
}

// Checks to see if a current twitch client has a valid app access token.
// If we don't have a current token, or we do and its expired, request a new one.
// If we have a current token that hasn't been validated in over an hour, validate it.
func (c *APIClient) ensureValidAppToken() error {
	c.appTokenMutex.RLock()
	noToken := c.appToken == ""
	tokenExpired := time.Now().Add(appTokenBufferTime).After(c.appTokenExpiry)
	tokenNotValidated := time.Now().After(c.appTokenNextValidationCheck)
	c.appTokenMutex.RUnlock()

	if noToken || tokenExpired {
		return c.requestAppAccessToken()
	}

	if tokenNotValidated {
		c.appTokenMutex.RLock()
		err := c.validateOAuthToken(c.appToken)
		if err != nil {
			c.appTokenMutex.RUnlock()
			return err
		}

		// Set app access token to be validated again on an hourly basis as suggested by Twitch.
		c.appTokenNextValidationCheck = time.Now().Add(time.Duration(time.Hour))
		c.appTokenMutex.RUnlock()
	}

	return nil
}

// Requests an OAuth app access token using the client credentials grant flow
func (c *APIClient) requestAppAccessToken() error {
	requrl := "https://id.twitch.tv/oauth2/token"

	c.appTokenMutex.Lock()
	defer c.appTokenMutex.Unlock()

	// If we already have a valid token, continue using it.
	if c.appToken != "" && time.Now().Add(appTokenBufferTime).Before(c.appTokenExpiry) {
		return nil
	}

	data := url.Values{}
	data.Set("client_id", c.clientID)
	data.Set("client_secret", c.clientSecret)
	data.Set("grant_type", "client_credentials")

	req, err := http.NewRequest("POST", requrl, bytes.NewBufferString(data.Encode()))
	if err != nil {
		return fmt.Errorf("failed to create oauth app token request: %w", err)
	}

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to request oauth app token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return APIError{
			Status:  resp.StatusCode,
			Message: fmt.Sprintf("OAuth token request failed: %s", string(body)),
		}
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return fmt.Errorf("failed to parse token response body into struct: %w", err)
	}

	// Update our twitch client's struct with a newly refreshed OAuth token.
	c.appToken = tokenResp.AccessToken
	c.appTokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	c.appTokenNextValidationCheck = time.Now().Add(time.Duration(time.Hour))

	return nil
}

// Check if a token that isn't explicitly expired due to time is still valid.
// Token's may be invalidated for reason's other than time expiration.
// https://dev.twitch.tv/docs/authentication/validate-tokens/#how-to-validate-a-token
func (c *APIClient) validateOAuthToken(token string) error {
	requrl := "https://id.twitch.tv/oauth2/validate"

	if c.appToken == "" {
		return fmt.Errorf("empty oauth token")
	}

	req, err := http.NewRequest("GET", requrl, nil)
	if err != nil {
		return fmt.Errorf("failed to create /validate oauth token request: %w", err)
	}

	req.Header.Add("Authorization", "OAuth "+token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send GET request to validate oauth token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return APIError{
			Status:  resp.StatusCode,
			Message: fmt.Sprintf("OAuth token failed validation: %s", string(body)),
		}
	}

	return nil
}

func (c *APIClient) GetUsers(params GetUsersParams) (*APIGetUsersResponse, error) {
	requrl := "https://api.twitch.tv/helix/users"

	err := c.ensureValidAppToken()
	if err != nil {
		return nil, fmt.Errorf("unable to use a valid oauth access token: %w", err)
	}

	query := url.Values{}
	for _, id := range params.UserIDs {
		query.Add("id", id)
	}

	for _, login := range params.UserLogins {
		query.Add("login", login)
	}

	requrl += ("?" + query.Encode())
	req, err := http.NewRequest("GET", requrl, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create new request: %w", err)
	}

	c.appTokenMutex.RLock()
	req.Header.Add("Authorization", "Bearer "+c.appToken)
	c.appTokenMutex.RUnlock()
	req.Header.Add("Client-Id", c.clientID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, APIError{
			Status:  resp.StatusCode,
			Message: string(body),
		}
	}

	var getUsersResp APIGetUsersResponse
	if err := json.NewDecoder(resp.Body).Decode(&getUsersResp); err != nil {
		return nil, fmt.Errorf("failed to parse GetStreams response json: %w", err)
	}

	getUsersResp.userIdMap = make(map[string]*APIGetUsersData, len(getUsersResp.Data))
	getUsersResp.userLoginMap = make(map[string]*APIGetUsersData, len(getUsersResp.Data))
	for i, user := range getUsersResp.Data {
		getUsersResp.userIdMap[user.UserID] = &getUsersResp.Data[i]
		getUsersResp.userLoginMap[user.UserLogin] = &getUsersResp.Data[i]
	}

	return &getUsersResp, nil
}

func (c *APIClient) GetStreams(params GetStreamsParams) (*APIGetStreamsResponse, error) {
	requrl := "https://api.twitch.tv/helix/streams"

	err := c.ensureValidAppToken()
	if err != nil {
		return nil, fmt.Errorf("unable to use a valid oauth access token: %w", err)
	}

	query := url.Values{}
	for _, id := range params.UserIDs {
		query.Add("user_id", id)
	}

	for _, login := range params.UserLogins {
		query.Add("user_login", login)
	}

	// QUESTION: Does this restrict reruns and hosts from getting fetched?
	query.Add("type", "live")

	requrl += ("?" + query.Encode())
	req, err := http.NewRequest("GET", requrl, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create new GetStreams request: %w", err)
	}

	c.appTokenMutex.RLock()
	req.Header.Add("Authorization", "Bearer "+c.appToken)
	c.appTokenMutex.RUnlock()
	req.Header.Add("Client-Id", c.clientID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to do GetStreams request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, APIError{
			Status:  resp.StatusCode,
			Message: string(body),
		}
	}

	var getStreamsResp APIGetStreamsResponse
	if err := json.NewDecoder(resp.Body).Decode(&getStreamsResp); err != nil {
		return nil, fmt.Errorf("failed to parse GetStreams response json: %w", err)
	}

	return &getStreamsResp, nil
}

// Request a playback access token for a specific streamer from their login username using twitch GQL
// A playback access token is needed to authenticate the request to get the m3u8 variant playlist for a live stream.
// Implementation based on streamlink's twitch plugin.
func (c *APIClient) getPlaybackAccessToken(userLogin string) (*StreamPlaybackAccessToken, error) {
	query := map[string]any{
		"operationName": "PlaybackAccessToken",
		"variables": map[string]any{
			"isLive":     true,
			"login":      userLogin,
			"isVod":      false,
			"vodID":      "",
			"playerType": "embed",
		},
		"extensions": map[string]any{
			"persistedQuery": map[string]any{
				"version":    1,
				"sha256Hash": "0828119ded1c13477966434e15800ff57ddacf13ba1911c129dc2200705b0712",
			},
		},
	}

	jsonData, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal PlaybackAccessToken gql query as json: %w", err)
	}

	req, err := http.NewRequest("POST", gqlURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create PlaybackAccessToken gql POST request: %w", err)
	}

	req.Header.Set("Client-ID", "kimne78kx3ncx6brgo4mv6wki5h1ko") // TODO: make this config/passed in
	req.Header.Set("Content-Type", "application/json")

	// If a user token is not provided, there will most likely be ad breaks in the recordings.
	if c.userToken != "" {
		req.Header.Set("Authorization", "OAuth "+c.userToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to do PlaybackAccessToken gql request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, APIError{
			Status:  resp.StatusCode,
			Message: string(body),
		}
	}

	var tokenResp GQLPlaybackAccessTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse PlaybackAccessToken GQL response json: %w", err)
	}

	return &tokenResp.Data.Token, nil
}

// Make the authenticated usher request url string to get the m3u8 variant playlist for a stream.
// Implementation based on streamlink's twitch plugin.
func makeUsherM3U8PlaylistUrl(userLogin string, token *StreamPlaybackAccessToken) string {
	usherUrl := fmt.Sprintf("https://usher.ttvnw.net/api/channel/hls/%s.m3u8", userLogin)

	params := url.Values{}
	params.Add("sig", token.Signature)
	params.Add("token", token.Value)
	params.Add("allow_source", "true")
	params.Add("fast_bread", "true")
	params.Add("player", "twitchweb")
	params.Add("p", strconv.Itoa(rand.IntN(999999))) // no clue what this does just referencing streamlink
	params.Add("type", "any")
	params.Add("allow_spectre", "false")

	// Prefetch HLS segments, attempting to always record as much stream as possible
	// in the event of an unintended end to a stream.
	params.Add("low_latency", "true")

	// This will essentially do nothing,
	// twitch turbo is required as of now to have no ads break sections in the HLS stream.
	params.Add("disable_ads", "true")

	usherUrl += ("?" + params.Encode())

	return usherUrl
}
