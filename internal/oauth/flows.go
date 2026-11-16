package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zenpo/fastrouter/internal/store"
)

const deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

func init() {
	// ---- Claude Code: authorization code + PKCE (9Router providers/claude.js) ----
	const claudeClient = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	const claudeToken = "https://api.anthropic.com/v1/oauth/token"
	register(&Spec{ID: "claude", Kind: KindBrowser, Risk: true, Rotates: true, RefreshLead: 4 * time.Hour,
		Start: func(ctx context.Context, e Env) (*Pending, error) {
			p := &Pending{Kind: KindBrowser, State: randHex(16), redirect: e.RedirectURI, Expires: time.Now().Add(10 * time.Minute),
				Hint: "If Claude shows a code instead of returning here, paste it (code#state) below."}
			var ch string
			p.verifier, ch = pkce(32)
			q := url.Values{"code": {"true"}, "client_id": {claudeClient}, "response_type": {"code"}, "redirect_uri": {e.RedirectURI},
				"scope": {"org:create_api_key user:profile user:inference"}, "code_challenge": {ch}, "code_challenge_method": {"S256"}, "state": {p.State}}
			p.URL = "https://claude.ai/oauth/authorize?" + q.Encode()
			return p, nil
		},
		Exchange: func(ctx context.Context, e Env, p *Pending, q url.Values) (*Result, error) {
			code, st := q.Get("code"), q.Get("state")
			if i := strings.IndexByte(code, '#'); i >= 0 {
				code, st = code[:i], firstNonEmpty(code[i+1:], st)
			}
			var t tokenResp
			_, err := postJSON(ctx, e.Client, claudeToken, map[string]string{"code": code, "state": firstNonEmpty(st, p.State),
				"grant_type": "authorization_code", "client_id": claudeClient, "redirect_uri": p.redirect, "code_verifier": p.verifier}, nil, &t)
			if err != nil {
				return nil, fmt.Errorf("token exchange failed: %w", err)
			}
			r := &Result{Name: "Claude account", Secrets: map[string]string{"accessToken": t.AccessToken, "refreshToken": t.RefreshToken}, ExpiresIn: t.ExpiresIn}
			var prof struct {
				Account struct {
					Email string `json:"email"`
				} `json:"account"`
			}
			if _, err := getJSON(ctx, e.Client, "https://api.anthropic.com/api/oauth/profile", map[string]string{"Authorization": "Bearer " + t.AccessToken,
				"anthropic-beta": "oauth-2025-04-20"}, &prof); err == nil && prof.Account.Email != "" {
				r.Email, r.Name = prof.Account.Email, prof.Account.Email
			}
			return r, nil
		},
		Refresh: func(ctx context.Context, e Env, c store.Connection) (*Result, error) {
			var t tokenResp
			if _, err := postJSON(ctx, e.Client, claudeToken, map[string]string{"grant_type": "refresh_token",
				"refresh_token": Secret(c, "refreshToken"), "client_id": claudeClient}, nil, &t); err != nil {
				return nil, refreshErr(err)
			}
			return &Result{Secrets: map[string]string{"accessToken": t.AccessToken, "refreshToken": t.RefreshToken}, ExpiresIn: t.ExpiresIn}, nil
		},
	})

	// ---- GitHub Copilot: device flow, then a short-lived Copilot token ----
	const ghClient = "Iv1.b507a08c87ecfe98"
	register(&Spec{ID: "github", Kind: KindDevice, Risk: true, RefreshLead: 5 * time.Minute,
		Start: func(ctx context.Context, e Env) (*Pending, error) {
			var d struct {
				DeviceCode      string `json:"device_code"`
				UserCode        string `json:"user_code"`
				VerificationURI string `json:"verification_uri"`
				ExpiresIn       int    `json:"expires_in"`
				Interval        int    `json:"interval"`
			}
			if _, err := postForm(ctx, e.Client, "https://github.com/login/device/code", url.Values{"client_id": {ghClient}, "scope": {"read:user"}}, nil, &d); err != nil {
				return nil, err
			}
			return deviceDefaults(&Pending{URL: d.VerificationURI, UserCode: d.UserCode, data: map[string]string{"device": d.DeviceCode}}, d.Interval, d.ExpiresIn), nil
		},
		Poll: func(ctx context.Context, e Env, p *Pending) (*Result, error) {
			var t tokenResp
			_, err := postForm(ctx, e.Client, "https://github.com/login/oauth/access_token", url.Values{"client_id": {ghClient},
				"device_code": {p.data["device"]}, "grant_type": {deviceGrant}}, nil, &t)
			if r, done := devicePending(t.Error, err); done {
				return nil, r
			}
			r := &Result{Secrets: map[string]string{"accessToken": t.AccessToken, "refreshToken": t.RefreshToken}}
			var u struct {
				Login, Name, Email string
				ID                 float64
			}
			_, _ = getJSON(ctx, e.Client, "https://api.github.com/user", ghHeaders("Bearer "+t.AccessToken), &u)
			r.Name, r.Email = firstNonEmpty(u.Login, u.Name, "GitHub account"), u.Email
			r.PSD = map[string]any{"githubLogin": u.Login, "githubUserId": u.ID}
			if err := copilotToken(ctx, e.Client, t.AccessToken, r); err != nil {
				return nil, fmt.Errorf("signed in, but GitHub gave no Copilot token (no Copilot subscription?): %w", err)
			}
			return r, nil
		},
		Refresh: func(ctx context.Context, e Env, c store.Connection) (*Result, error) {
			r := &Result{}
			if err := copilotToken(ctx, e.Client, Secret(c, "accessToken"), r); err != nil {
				return nil, refreshErr(err)
			}
			return r, nil
		},
	})

	// ---- Kimi Code: device flow (Anthropic-format coding API) ----
	const kimiClient = "17e5f671-d194-4dfb-9706-5516cb48c098"
	register(&Spec{ID: "kimi", Kind: KindDevice, Risk: true, Rotates: true, RefreshLead: 5 * time.Minute,
		Start: func(ctx context.Context, e Env) (*Pending, error) {
			dev := uuid4()
			var d struct {
				DC  string `json:"device_code"`
				UC  string `json:"user_code"`
				URI string `json:"verification_uri_complete"`
				EI  int    `json:"expires_in"`
				IV  int    `json:"interval"`
			}
			if _, err := postForm(ctx, e.Client, "https://auth.kimi.com/api/oauth/device_authorization", url.Values{"client_id": {kimiClient}}, kimiHeaders(dev), &d); err != nil {
				return nil, err
			}
			u := firstNonEmpty(d.URI, "https://www.kimi.com/code/authorize_device?user_code="+url.QueryEscape(d.UC))
			return deviceDefaults(&Pending{URL: u, UserCode: d.UC, data: map[string]string{"device": d.DC, "deviceId": dev}}, d.IV, d.EI), nil
		},
		Poll: func(ctx context.Context, e Env, p *Pending) (*Result, error) {
			var t tokenResp
			_, err := postForm(ctx, e.Client, "https://auth.kimi.com/api/oauth/token", url.Values{"grant_type": {deviceGrant},
				"client_id": {kimiClient}, "device_code": {p.data["device"]}}, kimiHeaders(p.data["deviceId"]), &t)
			if r, done := devicePending(t.Error, err); done {
				return nil, r
			}
			return &Result{Name: "Kimi account", Secrets: map[string]string{"accessToken": t.AccessToken, "refreshToken": t.RefreshToken},
				ExpiresIn: t.ExpiresIn, PSD: map[string]any{"authMethod": "device_code", "deviceId": p.data["deviceId"]}}, nil
		},
		Refresh: func(ctx context.Context, e Env, c store.Connection) (*Result, error) {
			var t tokenResp
			if _, err := postForm(ctx, e.Client, "https://auth.kimi.com/api/oauth/token", url.Values{"grant_type": {"refresh_token"},
				"refresh_token": {Secret(c, "refreshToken")}, "client_id": {kimiClient}}, kimiHeaders(Meta(c, "deviceId")), &t); err != nil {
				return nil, refreshErr(err)
			}
			return &Result{Secrets: map[string]string{"accessToken": t.AccessToken, "refreshToken": t.RefreshToken}, ExpiresIn: t.ExpiresIn}, nil
		},
	})

	// ---- Kilo Code: device codes; the token does not expire ----
	register(&Spec{ID: "kilocode", Kind: KindDevice,
		Start: func(ctx context.Context, e Env) (*Pending, error) {
			var d struct {
				Code            string `json:"code"`
				VerificationURL string `json:"verificationUrl"`
				ExpiresIn       int    `json:"expiresIn"`
			}
			if _, err := postJSON(ctx, e.Client, "https://api.kilo.ai/api/device-auth/codes", map[string]any{}, nil, &d); err != nil {
				return nil, err
			}
			return deviceDefaults(&Pending{URL: d.VerificationURL, UserCode: d.Code, data: map[string]string{"device": d.Code}}, 3, d.ExpiresIn), nil
		},
		Poll: func(ctx context.Context, e Env, p *Pending) (*Result, error) {
			var d struct {
				Status, Token, UserEmail string
			}
			code, err := getJSON(ctx, e.Client, "https://api.kilo.ai/api/device-auth/codes/"+url.PathEscape(p.data["device"]), nil, &d)
			switch {
			case code == http.StatusAccepted:
				return nil, ErrPending
			case code == http.StatusForbidden:
				return nil, errors.New("sign-in was denied")
			case code == http.StatusGone:
				return nil, errors.New("the code expired; start again")
			case err != nil:
				return nil, err
			case d.Status != "approved" || d.Token == "":
				return nil, ErrPending
			}
			r := &Result{Name: firstNonEmpty(d.UserEmail, "Kilo account"), Email: d.UserEmail, Secrets: map[string]string{"accessToken": d.Token}}
			var prof struct {
				Organizations []struct {
					ID string `json:"id"`
				} `json:"organizations"`
			}
			if _, err := getJSON(ctx, e.Client, "https://api.kilo.ai/api/profile", map[string]string{"Authorization": "Bearer " + d.Token}, &prof); err == nil && len(prof.Organizations) > 0 {
				r.PSD = map[string]any{"orgId": prof.Organizations[0].ID}
			}
			return r, nil
		},
	})

	// ---- Cline / ClinePass: browser sign-in, code carries base64 JSON tokens ----
	for _, id := range []string{"cline", "clinepass"} {
		register(&Spec{ID: id, Kind: KindBrowser, Rotates: true, RefreshLead: 5 * time.Minute,
			Start: func(ctx context.Context, e Env) (*Pending, error) {
				p := &Pending{Kind: KindBrowser, State: randHex(16), redirect: e.RedirectURI, Expires: time.Now().Add(10 * time.Minute)}
				cb := e.RedirectURI + "?state=" + p.State
				p.URL = "https://api.cline.bot/api/v1/auth/authorize?" + url.Values{"client_type": {"extension"}, "callback_url": {cb}, "redirect_uri": {cb}}.Encode()
				return p, nil
			},
			Exchange: func(ctx context.Context, e Env, p *Pending, q url.Values) (*Result, error) {
				return clineExchange(ctx, e, p, q.Get("code"))
			},
			Refresh: func(ctx context.Context, e Env, c store.Connection) (*Result, error) {
				var out struct {
					Data struct {
						AccessToken, RefreshToken, ExpiresAt string
					} `json:"data"`
				}
				if _, err := postJSON(ctx, e.Client, "https://api.cline.bot/api/v1/auth/refresh", map[string]string{"refreshToken": Secret(c, "refreshToken"),
					"grantType": "refresh_token", "clientType": "extension"}, nil, &out); err != nil {
					return nil, refreshErr(err)
				}
				return &Result{Secrets: map[string]string{"accessToken": clineToken(out.Data.AccessToken), "refreshToken": out.Data.RefreshToken},
					ExpiresIn: untilISO(out.Data.ExpiresAt)}, nil
			},
		})
	}

	// ---- CodeBuddy (Tencent): state polling ----
	for _, cb := range []struct{ id, base, domain, ua, platform string }{
		{"codebuddy-cn", "https://copilot.tencent.com", "copilot.tencent.com", "CLI/2.63.2 CodeBuddy/2.63.2", "CLI"},
		{"codebuddy-intl", "https://www.codebuddy.ai", "www.codebuddy.ai", "IDE/2.63.2 CodeBuddy/2.63.2", "ide"},
	} {
		cb := cb
		hdr := func(extra map[string]string) map[string]string {
			h := map[string]string{"User-Agent": cb.ua, "X-Requested-With": "XMLHttpRequest", "X-Domain": cb.domain, "X-Product": "SaaS"}
			for k, v := range extra {
				h[k] = v
			}
			return h
		}
		register(&Spec{ID: cb.id, Kind: KindDevice, Rotates: true, RefreshLead: 10 * time.Minute,
			Start: func(ctx context.Context, e Env) (*Pending, error) {
				var out struct {
					Code int `json:"code"`
					Msg  string
					Data struct{ State, AuthURL string } `json:"data"`
				}
				if _, err := postJSON(ctx, e.Client, cb.base+"/v2/plugin/auth/state?platform="+cb.platform, map[string]any{},
					hdr(map[string]string{"X-No-Authorization": "true", "X-No-User-Id": "true"}), &out); err != nil {
					return nil, err
				}
				if out.Code != 0 || out.Data.State == "" {
					return nil, fmt.Errorf("CodeBuddy sign-in error %d", out.Code)
				}
				return deviceDefaults(&Pending{URL: out.Data.AuthURL, data: map[string]string{"device": out.Data.State}}, 5, 600), nil
			},
			Poll: func(ctx context.Context, e Env, p *Pending) (*Result, error) {
				var out codebuddyTok
				if _, err := getJSON(ctx, e.Client, cb.base+"/v2/plugin/auth/token?state="+url.QueryEscape(p.data["device"]),
					hdr(map[string]string{"X-No-Authorization": "true", "X-No-User-Id": "true", "X-No-Enterprise-Id": "true", "X-No-Department-Info": "true"}), &out); err != nil {
					return nil, err
				}
				if out.Code == 11217 || (out.Code == 0 && out.Data.AccessToken == "") {
					return nil, ErrPending
				}
				if out.Code != 0 {
					return nil, fmt.Errorf("CodeBuddy sign-in error %d", out.Code)
				}
				return out.result("CodeBuddy account"), nil
			},
			Refresh: func(ctx context.Context, e Env, c store.Connection) (*Result, error) {
				var out codebuddyTok
				if _, err := postJSON(ctx, e.Client, cb.base+"/v2/plugin/auth/token/refresh", map[string]any{},
					hdr(map[string]string{"X-Refresh-Token": Secret(c, "refreshToken"), "X-Auth-Refresh-Source": "plugin"}), &out); err != nil {
					return nil, refreshErr(err)
				}
				if out.Code != 0 || out.Data.AccessToken == "" {
					return nil, ErrReauth
				}
				return out.result(""), nil
			},
		})
	}

	// ---- MiniMax Code: device code with PKCE ----
	for _, mm := range []struct{ id, account string }{{"minimax-code", "https://account.minimax.cn"}, {"minimax-code-global", "https://account.minimax.io"}} {
		mm := mm
		register(&Spec{ID: mm.id, Kind: KindDevice, Rotates: true, RefreshLead: 10 * time.Minute,
			Start: func(ctx context.Context, e Env) (*Pending, error) {
				ver, ch := pkce(32)
				var d map[string]any
				if _, err := postForm(ctx, e.Client, mm.account+"/oauth2/device/code", url.Values{"client_id": {"mcode-public"}, "scope": {"agent.default"},
					"audience": {"agent-backend"}, "code_challenge": {ch}, "code_challenge_method": {"S256"}}, nil, &d); err != nil {
					return nil, err
				}
				uc, dc := anyStr(d["user_code"]), anyStr(d["device_code"])
				uri := firstNonEmpty(anyStr(d["verification_uri_complete"]), anyStr(d["verification_uri"]), anyStr(d["verification_url"]))
				if uc == "" || uri == "" {
					return nil, errors.New("MiniMax gave no device code")
				}
				p := &Pending{URL: uri, UserCode: uc, verifier: ver, data: map[string]string{"device": firstNonEmpty(dc, uc)}}
				if dc == "" {
					p.data["byUser"] = "1"
				}
				return deviceDefaults(p, 5, 600), nil
			},
			Poll: func(ctx context.Context, e Env, p *Pending) (*Result, error) {
				f := url.Values{"grant_type": {deviceGrant}, "client_id": {"mcode-public"}, "code_verifier": {p.verifier}}
				if p.data["byUser"] != "" {
					f.Set("user_code", p.data["device"])
				} else {
					f.Set("device_code", p.data["device"])
				}
				var d map[string]any
				_, err := postForm(ctx, e.Client, mm.account+"/oauth2/token", f, nil, &d)
				switch anyStr(d["status"]) {
				case "pending", "slow_down":
					return nil, ErrPending
				case "denied", "access_denied":
					return nil, errors.New("sign-in was denied")
				case "expired", "expired_token":
					return nil, errors.New("the code expired; start again")
				}
				if r, done := devicePending(anyStr(d["error"]), err); done {
					return nil, r
				}
				if anyStr(d["access_token"]) == "" {
					return nil, ErrPending
				}
				exp, _ := d["expires_in"].(float64)
				return &Result{Name: "MiniMax account", Secrets: map[string]string{"accessToken": anyStr(d["access_token"]), "refreshToken": anyStr(d["refresh_token"])}, ExpiresIn: int(exp)}, nil
			},
			Refresh: func(ctx context.Context, e Env, c store.Connection) (*Result, error) {
				var t tokenResp
				if _, err := postForm(ctx, e.Client, mm.account+"/oauth2/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {Secret(c, "refreshToken")},
					"client_id": {"mcode-public"}, "scope": {"agent.default"}, "audience": {"agent-backend"}}, nil, &t); err != nil {
					return nil, refreshErr(err)
				}
				return &Result{Secrets: map[string]string{"accessToken": t.AccessToken, "refreshToken": t.RefreshToken}, ExpiresIn: t.ExpiresIn}, nil
			},
		})
	}

	// ---- Grok CLI (xAI Grok Build): device code ----
	const xaiClient = "b1a00492-073a-47ea-816f-4c329264a828"
	pager := map[string]string{"User-Agent": "grok-pager/1.0.44 grok-shell/1.0.44 (linux; x86_64)"}
	register(&Spec{ID: "grok-cli", Kind: KindDevice, Risk: true, Rotates: true, RefreshLead: 5 * time.Minute,
		Start: func(ctx context.Context, e Env) (*Pending, error) {
			var d struct {
				DeviceCode string `json:"device_code"`
				UserCode   string `json:"user_code"`
				URI        string `json:"verification_uri"`
				URIc       string `json:"verification_uri_complete"`
				ExpiresIn  int    `json:"expires_in"`
				Interval   int    `json:"interval"`
			}
			if _, err := postForm(ctx, e.Client, "https://auth.x.ai/oauth2/device/code", url.Values{"client_id": {xaiClient},
				"scope": {"openid profile email offline_access grok-cli:access api:access conversations:read conversations:write"}, "referrer": {"grok-build"}}, pager, &d); err != nil {
				return nil, err
			}
			return deviceDefaults(&Pending{URL: firstNonEmpty(d.URIc, d.URI), UserCode: d.UserCode, data: map[string]string{"device": d.DeviceCode}}, d.Interval, d.ExpiresIn), nil
		},
		Poll: func(ctx context.Context, e Env, p *Pending) (*Result, error) {
			var t tokenResp
			_, err := postForm(ctx, e.Client, "https://auth.x.ai/oauth2/token", url.Values{"grant_type": {deviceGrant}, "device_code": {p.data["device"]},
				"client_id": {xaiClient}}, pager, &t)
			if r, done := devicePending(t.Error, err); done {
				return nil, r
			}
			email := firstNonEmpty(jwtClaim(t.IDToken, "email"), jwtClaim(t.AccessToken, "email"))
			r := &Result{Name: firstNonEmpty(email, "Grok account"), Email: email, ExpiresIn: t.ExpiresIn,
				Secrets: map[string]string{"accessToken": t.AccessToken, "refreshToken": t.RefreshToken}, PSD: map[string]any{"authMethod": "device_code", "email": email}}
			var u struct {
				UserID      string `json:"userId"`
				PrincipalID string `json:"principalId"`
			}
			h := map[string]string{"Authorization": "Bearer " + t.AccessToken, "x-xai-token-auth": "xai-grok-cli", "x-grok-client-version": "1.0.44"}
			for k, v := range pager {
				h[k] = v
			}
			if _, err := getJSON(ctx, e.Client, "https://cli-chat-proxy.grok.com/v1/user", h, &u); err == nil {
				r.PSD["userId"] = firstNonEmpty(u.UserID, u.PrincipalID)
			}
			return r, nil
		},
		Refresh: func(ctx context.Context, e Env, c store.Connection) (*Result, error) {
			var t tokenResp
			if _, err := postForm(ctx, e.Client, "https://auth.x.ai/oauth2/token", url.Values{"grant_type": {"refresh_token"},
				"refresh_token": {Secret(c, "refreshToken")}, "client_id": {xaiClient}}, pager, &t); err != nil {
				return nil, refreshErr(err)
			}
			return &Result{Secrets: map[string]string{"accessToken": t.AccessToken, "refreshToken": t.RefreshToken}, ExpiresIn: t.ExpiresIn}, nil
		},
	})
}

// devicePending maps a device-token poll answer: (ErrPending|error, true) to
// keep waiting or stop, (nil, false) when a token arrived.
func devicePending(code string, err error) (error, bool) {
	var he *httpError
	if code == "" && errors.As(err, &he) {
		code = he.Code
	}
	switch code {
	case "authorization_pending", "slow_down":
		return ErrPending, true
	case "":
		if err != nil {
			return err, true
		}
		return nil, false
	case "access_denied":
		return errors.New("sign-in was denied"), true
	case "expired_token":
		return errors.New("the code expired; start again"), true
	}
	return fmt.Errorf("sign-in failed (%s)", code), true
}

func ghHeaders(auth string) map[string]string {
	return map[string]string{"Authorization": auth, "X-GitHub-Api-Version": "2022-11-28", "User-Agent": "GitHubCopilotChat/0.38.0",
		"Editor-Version": "vscode/1.110.0", "Editor-Plugin-Version": "copilot-chat/0.38.0"}
}

func copilotToken(ctx context.Context, c *http.Client, ghToken string, r *Result) error {
	if ghToken == "" {
		return ErrReauth
	}
	var t struct {
		Token     string  `json:"token"`
		ExpiresAt float64 `json:"expires_at"`
	}
	if _, err := getJSON(ctx, c, "https://api.github.com/copilot_internal/v2/token", ghHeaders("token "+ghToken), &t); err != nil {
		return err
	}
	if t.Token == "" {
		return errors.New("empty Copilot token")
	}
	if r.Secrets == nil {
		r.Secrets = map[string]string{}
	}
	r.Secrets["copilotToken"] = t.Token
	if t.ExpiresAt > 0 {
		r.ExpiresIn = int(time.Until(time.Unix(int64(t.ExpiresAt), 0)).Seconds())
	}
	return nil
}

func clineExchange(ctx context.Context, e Env, p *Pending, code string) (*Result, error) {
	var tok struct {
		AccessToken, RefreshToken, Email, FirstName, LastName, ExpiresAt string
	}
	raw := code
	if m := len(raw) % 4; m != 0 {
		raw += strings.Repeat("=", 4-m)
	}
	dec, derr := base64.StdEncoding.DecodeString(raw)
	if derr != nil {
		dec, derr = base64.URLEncoding.DecodeString(raw)
	}
	if i := strings.LastIndexByte(string(dec), '}'); derr == nil && i > 0 && json.Unmarshal(dec[:i+1], &tok) == nil && tok.AccessToken != "" {
	} else {
		var out struct {
			Data struct {
				AccessToken, RefreshToken, ExpiresAt string
				UserInfo                             struct{ Email string } `json:"userInfo"`
			} `json:"data"`
		}
		if _, err := postJSON(ctx, e.Client, "https://api.cline.bot/api/v1/auth/token", map[string]string{"grant_type": "authorization_code",
			"code": code, "client_type": "extension", "redirect_uri": p.redirect}, nil, &out); err != nil {
			return nil, fmt.Errorf("token exchange failed: %w", err)
		}
		tok.AccessToken, tok.RefreshToken, tok.ExpiresAt, tok.Email = out.Data.AccessToken, out.Data.RefreshToken, out.Data.ExpiresAt, out.Data.UserInfo.Email
	}
	if tok.AccessToken == "" {
		return nil, errors.New("Cline returned no token")
	}
	exp := untilISO(tok.ExpiresAt)
	if exp == 0 {
		exp = 3600
	}
	return &Result{Name: firstNonEmpty(tok.Email, "Cline account"), Email: tok.Email, ExpiresIn: exp,
		Secrets: map[string]string{"accessToken": tok.AccessToken, "refreshToken": tok.RefreshToken},
		PSD:     map[string]any{"firstName": tok.FirstName, "lastName": tok.LastName}}, nil
}

func untilISO(s string) int {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		if n, e := strconv.ParseInt(s, 10, 64); e == nil && n > 0 {
			if n > 1e12 {
				n /= 1000
			}
			t = time.Unix(n, 0)
		} else {
			return 0
		}
	}
	return max(1, int(time.Until(t).Seconds()))
}

type codebuddyTok struct {
	Code int `json:"code"`
	Data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
	} `json:"data"`
}

func (t codebuddyTok) result(name string) *Result {
	exp := t.Data.ExpiresIn
	if exp == 0 {
		exp = 86400
	}
	return &Result{Name: name, Secrets: map[string]string{"accessToken": t.Data.AccessToken, "refreshToken": t.Data.RefreshToken}, ExpiresIn: exp}
}

// ---- iFlow: authorization code; the account's API key comes from userinfo ----
const (
	iflowClient = "10009311001"
	iflowSecret = "4Z3YjXycVsQvyGF1etiNlIBB4RsqSDtW" // public CLI client secret (same as iFlow CLI / 9Router)
	iflowToken  = "https://iflow.cn/oauth/token"
)

func iflowBasic() map[string]string {
	return map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(iflowClient+":"+iflowSecret))}
}

func iflowUser(ctx context.Context, e Env, access string, r *Result) error {
	var u struct {
		Success bool `json:"success"`
		Data    struct {
			APIKey, Email, Phone, Nickname string
		} `json:"data"`
	}
	if _, err := getJSON(ctx, e.Client, "https://iflow.cn/api/oauth/getUserInfo?accessToken="+url.QueryEscape(access), nil, &u); err != nil {
		return err
	}
	if !u.Success || strings.TrimSpace(u.Data.APIKey) == "" {
		return errors.New("iFlow returned no API key for this account")
	}
	r.Secrets["apiKey"] = strings.TrimSpace(u.Data.APIKey)
	r.Email = firstNonEmpty(u.Data.Email, u.Data.Phone)
	r.Name = firstNonEmpty(u.Data.Nickname, r.Email, "iFlow account")
	return nil
}

func init() {
	register(&Spec{ID: "iflow", Kind: KindBrowser, Rotates: true, RefreshLead: 24 * time.Hour,
		Start: func(ctx context.Context, e Env) (*Pending, error) {
			p := &Pending{Kind: KindBrowser, State: randHex(16), redirect: e.RedirectURI, Expires: time.Now().Add(10 * time.Minute)}
			p.URL = "https://iflow.cn/oauth?" + url.Values{"loginMethod": {"phone"}, "type": {"phone"}, "redirect": {e.RedirectURI},
				"state": {p.State}, "client_id": {iflowClient}}.Encode()
			return p, nil
		},
		Exchange: func(ctx context.Context, e Env, p *Pending, q url.Values) (*Result, error) {
			var t tokenResp
			if _, err := postForm(ctx, e.Client, iflowToken, url.Values{"grant_type": {"authorization_code"}, "code": {q.Get("code")},
				"redirect_uri": {p.redirect}, "client_id": {iflowClient}, "client_secret": {iflowSecret}}, iflowBasic(), &t); err != nil {
				return nil, fmt.Errorf("token exchange failed: %w", err)
			}
			r := &Result{Secrets: map[string]string{"accessToken": t.AccessToken, "refreshToken": t.RefreshToken}, ExpiresIn: t.ExpiresIn}
			return r, iflowUser(ctx, e, t.AccessToken, r)
		},
		Refresh: func(ctx context.Context, e Env, c store.Connection) (*Result, error) {
			var t tokenResp
			if _, err := postForm(ctx, e.Client, iflowToken, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {Secret(c, "refreshToken")},
				"client_id": {iflowClient}, "client_secret": {iflowSecret}}, iflowBasic(), &t); err != nil {
				return nil, refreshErr(err)
			}
			r := &Result{Secrets: map[string]string{"accessToken": t.AccessToken, "refreshToken": t.RefreshToken}, ExpiresIn: t.ExpiresIn}
			_ = iflowUser(ctx, e, t.AccessToken, r) // keeps the old key if userinfo fails
			r.Name = ""
			return r, nil
		},
	})
}
