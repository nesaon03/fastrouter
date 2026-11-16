package login

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GLM (Z.ai coding plan) endpoints, from 9Router's glm provider.
const (
	GLMChatURL     = "https://api.z.ai/api/coding/paas/v4/chat/completions"
	GLMMessagesURL = "https://api.z.ai/api/anthropic/v1/messages?beta=true"
	glmKeyName     = "zcode-api-key"
)

// GLMDevice is a started Z.ai sign-in: the user opens URL, FastRouter polls.
type GLMDevice struct {
	FlowID    string
	URL       string
	Interval  time.Duration
	ExpiresAt time.Time
	pollToken string // bearer for init/poll; never shown
}

// GLMResult is a finished sign-in. APIKey is the long-lived coding-plan key
// (Z.ai has no refresh grant: when it stops working, sign in again).
type GLMResult struct {
	APIKey, JWT, BusinessToken string
	Email, Name, UserID        string
}

// ErrPending means the user has not finished signing in yet.
var ErrPending = errors.New("authorization pending")

type zEnvelope struct {
	Code    any             `json:"code"`
	Msg     string          `json:"msg"`
	Success *bool           `json:"success"`
	Data    json.RawMessage `json:"data"`
}

func (z *zEnvelope) ok() bool {
	if z.Success != nil && !*z.Success {
		return false
	}
	switch c := z.Code.(type) {
	case nil:
		return true
	case float64:
		return c == 0 || c == 200
	case string:
		return c == "0" || c == "200"
	}
	return false
}

func zDo(c *http.Client, req *http.Request, label string, out any) error {
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("z.ai %s: %w", label, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("z.ai %s failed (HTTP %d)", label, resp.StatusCode)
	}
	var env zEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return fmt.Errorf("z.ai %s: response is not JSON", label)
	}
	if !env.ok() {
		return fmt.Errorf("z.ai %s: %s", label, firstNonEmpty(env.Msg, "business error"))
	}
	data := env.Data
	if len(data) == 0 || string(data) == "null" {
		data = b
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// GLMStart begins the ZCode CLI sign-in (init returns a server-made authorize URL).
func (e Endpoints) GLMStart(ctx context.Context, c *http.Client) (*GLMDevice, error) {
	d := &GLMDevice{pollToken: RandomID(32)}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.GLMInit, strings.NewReader(`{"provider":"zai"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.pollToken)
	var data struct {
		AuthorizeURL string  `json:"authorize_url"`
		FlowID       string  `json:"flow_id"`
		Interval     float64 `json:"poll_interval_sec"`
		ExpiresAt    float64 `json:"expires_at"`
	}
	if err := zDo(c, req, "sign-in start", &data); err != nil {
		return nil, err
	}
	if data.FlowID == "" || data.AuthorizeURL == "" {
		return nil, errors.New("z.ai sign-in start: missing flow_id/authorize_url")
	}
	d.FlowID, d.URL = data.FlowID, data.AuthorizeURL
	d.Interval = time.Duration(max(data.Interval, 3)) * time.Second
	d.ExpiresAt = time.Now().Add(5 * time.Minute)
	if data.ExpiresAt > 0 {
		ms := data.ExpiresAt
		if ms < 1e12 {
			ms *= 1000
		}
		d.ExpiresAt = time.UnixMilli(int64(ms))
	}
	return d, nil
}

// GLMCheck checks the sign-in once. It returns ErrPending until the user is done.
func (e Endpoints) GLMCheck(ctx context.Context, c *http.Client, d *GLMDevice) (*GLMResult, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.GLMPoll+"/"+url.PathEscape(d.FlowID), nil)
	req.Header.Set("Authorization", "Bearer "+d.pollToken)
	var data map[string]any
	if err := zDo(c, req, "sign-in poll", &data); err != nil {
		return nil, err
	}
	switch data["status"] {
	case "ready":
	case "failed":
		return nil, errors.New("z.ai sign-in failed or was cancelled")
	default:
		return nil, ErrPending
	}
	zai, _ := data["zai"].(map[string]any)
	tok := firstNonEmpty(str(zai, "access_token"), firstNonEmpty(str(zai, "accessToken"), firstNonEmpty(str(data, "accessToken"), str(data, "access_token"))))
	if tok == "" {
		return nil, errors.New("z.ai sign-in: no access token in the answer")
	}
	res := &GLMResult{JWT: str(data, "token")}
	if u, _ := data["user"].(map[string]any); u != nil {
		res.Email, res.Name = str(u, "email"), str(u, "name")
		switch id := u["user_id"].(type) {
		case string:
			res.UserID = id
		case float64:
			res.UserID = strconv.FormatInt(int64(id), 10)
		}
	}
	var err error
	res.APIKey, res.BusinessToken, err = e.glmPlanKey(ctx, c, tok)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// glmPlanKey turns the Z.ai OAuth token into the coding-plan API key, the
// same way the official ZCode CLI does (business login, find or create the
// "zcode-api-key" key in the default org/project, copy its secret).
func (e Endpoints) glmPlanKey(ctx context.Context, c *http.Client, zaiToken string) (key, business string, err error) {
	body, _ := json.Marshal(map[string]string{"token": zaiToken})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.GLMBusinessLogin, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	var login struct {
		AccessToken  string `json:"access_token"`
		AccessToken2 string `json:"accessToken"`
	}
	if err := zDo(c, req, "business login", &login); err != nil {
		return "", "", err
	}
	business = strings.TrimSpace(firstNonEmpty(login.AccessToken, login.AccessToken2))
	if business == "" {
		return "", "", errors.New("z.ai business login: no access_token")
	}
	get := func(method, u string, in, out any) error {
		var rd io.Reader
		if in != nil {
			b, _ := json.Marshal(in)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequestWithContext(ctx, method, u, rd)
		req.Header.Set("Authorization", "Bearer "+business)
		req.Header.Set("Content-Type", "application/json")
		return zDo(c, req, "coding-plan key", out)
	}
	var info struct {
		Organizations []struct {
			OrganizationID   any    `json:"organizationId"`
			OrganizationName string `json:"organizationName"`
			Projects         []struct {
				ProjectID   any    `json:"projectId"`
				ProjectName string `json:"projectName"`
				ProjectType any    `json:"projectType"`
			} `json:"projects"`
		} `json:"organizations"`
	}
	if err := get(http.MethodGet, e.GLMAPI+"/api/biz/customer/getCustomerInfo", nil, &info); err != nil {
		return "", "", err
	}
	org, proj := "", ""
	for pass := 0; pass < 2 && proj == ""; pass++ {
		for _, o := range info.Organizations {
			if anyStr(o.OrganizationID) == "" || (pass == 0 && !isDefaultName(o.OrganizationName)) {
				continue
			}
			for _, p := range o.Projects {
				if anyStr(p.ProjectType) == "2" || anyStr(p.ProjectID) == "" {
					continue // team project
				}
				if proj == "" || isDefaultName(p.ProjectName) {
					org, proj = anyStr(o.OrganizationID), anyStr(p.ProjectID)
				}
			}
			if proj != "" {
				break
			}
		}
	}
	if proj == "" {
		return "", "", errors.New("could not find the Z.ai organization and project for the coding plan")
	}
	list := e.GLMAPI + "/api/biz/v1/organization/" + url.PathEscape(org) + "/projects/" + url.PathEscape(proj) + "/api_keys"
	var keys []struct {
		Name   string `json:"name"`
		APIKey string `json:"apiKey"`
	}
	if err := get(http.MethodGet, list, nil, &keys); err != nil {
		return "", "", err
	}
	id := ""
	for _, k := range keys {
		if k.Name == glmKeyName {
			id = strings.TrimSpace(k.APIKey)
		}
	}
	if id == "" {
		var created struct {
			APIKey string `json:"apiKey"`
		}
		if err := get(http.MethodPost, list, map[string]string{"name": glmKeyName}, &created); err != nil {
			return "", "", err
		}
		id = strings.TrimSpace(created.APIKey)
	}
	if id == "" {
		return "", "", errors.New("z.ai api key list had no apiKey")
	}
	var sec struct {
		SecretKey string `json:"secretKey"`
	}
	if err := get(http.MethodGet, list+"/copy/"+url.PathEscape(id), nil, &sec); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(sec.SecretKey) == "" {
		return "", "", errors.New("z.ai api key copy had no secretKey")
	}
	return id + "." + strings.TrimSpace(sec.SecretKey), business, nil
}

func isDefaultName(n string) bool {
	n = strings.ToLower(strings.TrimSpace(n))
	return strings.Contains(n, "默认机构") || strings.Contains(n, "默认项目") || n == "default"
}

func anyStr(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case float64:
		return strconv.FormatInt(int64(x), 10)
	}
	return ""
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
