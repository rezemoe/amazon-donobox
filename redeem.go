package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

var (
	// ErrCardRejected means Amazon refused the code (invalid, already redeemed, ...).
	ErrCardRejected = errors.New("rejected by amazon")
	// ErrNotSubmitted means the code was never applied (session expired, captcha, page
	// didn't load), so it is safe to retry later.
	ErrNotSubmitted = errors.New("not submitted")
)

// Redeemer applies a gift card code to the account. It returns Amazon's confirmation
// text on success. Errors not wrapping ErrCardRejected or ErrNotSubmitted mean the
// outcome is unknown.
type Redeemer interface {
	Redeem(ctx context.Context, code string) (string, error)
}

const (
	redeemURL = "https://www.amazon.de/gc/redeem"
	browserUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
)

var (
	codeInputSel   = chromedp.CSS("#gc-redemption-input")
	applyButtonSel = chromedp.CSS("#gc-redemption-apply-button")
)

// pageStateJS reports what the redeem page currently shows. Amazon pre-renders hidden
// alerts, so only visible ones count.
const pageStateJS = `(() => {
	const visible = el => el && el.offsetParent !== null && el.innerText.trim() !== '';
	const first = sel => [...document.querySelectorAll(sel)].find(visible);
	if (location.pathname.startsWith('/ap/')) return {state: 'signin', text: ''};
	if (document.querySelector('#captchacharacters, form[action*="validateCaptcha"]')) return {state: 'captcha', text: ''};
	const ok = first('.a-alert-success');
	if (ok) return {state: 'success', text: ok.innerText.trim()};
	const err = first('.a-alert-error, .a-alert-inline-error');
	if (err) return {state: 'error', text: err.innerText.trim()};
	return {state: '', text: ''};
})()`

type pageState struct {
	State string `json:"state"`
	Text  string `json:"text"`
}

// AmazonRedeemer drives a headless Chrome logged into amazon.de.
type AmazonRedeemer struct {
	browserCtx context.Context
	cancel     context.CancelFunc
}

func NewAmazonRedeemer(cfg Config) (*AmazonRedeemer, error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserDataDir(cfg.ChromeProfileDir),
		chromedp.UserAgent(browserUA),
		chromedp.Flag("lang", "de-DE"),
		chromedp.Flag("disable-dev-shm-usage", true),
	)
	if cfg.ChromePath != "" {
		opts = append(opts, chromedp.ExecPath(cfg.ChromePath))
	}
	if cfg.ChromeNoSandbox {
		opts = append(opts, chromedp.NoSandbox)
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	cancel := func() { cancelBrowser(); cancelAlloc() }

	if err := chromedp.Do(browserCtx); err != nil {
		cancel()
		return nil, fmt.Errorf("start chrome: %w", err)
	}
	if err := importCookies(browserCtx, cfg.CookiesPath); err != nil {
		cancel()
		return nil, err
	}
	return &AmazonRedeemer{browserCtx: browserCtx, cancel: cancel}, nil
}

func (r *AmazonRedeemer) Close() { r.cancel() }

// exportedCookie matches the JSON export of common browser cookie extensions (e.g. Cookie-Editor).
type exportedCookie struct {
	Name           string  `json:"name"`
	Value          string  `json:"value"`
	Domain         string  `json:"domain"`
	Path           string  `json:"path"`
	Secure         bool    `json:"secure"`
	HTTPOnly       bool    `json:"httpOnly"`
	ExpirationDate float64 `json:"expirationDate"`
}

// importCookies loads an exported amazon.de session into the browser profile. The profile
// is persisted, so this is only needed initially or when the session has expired.
func importCookies(ctx context.Context, path string) error {
	if path == "" {
		return nil
	}
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		log.Printf("no cookie file at %s, using the existing browser profile session", path)
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var exported []exportedCookie
	if err := json.Unmarshal(data, &exported); err != nil {
		return fmt.Errorf("parse cookie file: %w", err)
	}
	var params []*network.CookieParam
	for _, c := range exported {
		if !strings.HasSuffix(c.Domain, "amazon.de") {
			continue
		}
		p := &network.CookieParam{
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Secure: c.Secure, HTTPOnly: c.HTTPOnly,
		}
		if c.ExpirationDate > 0 {
			p.Expires = cdp.TimeSinceEpoch(c.ExpirationDate)
		}
		params = append(params, p)
	}
	if len(params) == 0 {
		return errors.New("cookie file contains no amazon.de cookies")
	}
	if _, err := chromedp.Call(ctx, network.SetCookies, network.SetCookiesParams{Cookies: params}); err != nil {
		return fmt.Errorf("import cookies: %w", err)
	}
	log.Printf("imported %d amazon.de cookies", len(params))
	return nil
}

func (r *AmazonRedeemer) Redeem(ctx context.Context, code string) (string, error) {
	tab, cancelTab := chromedp.NewContext(r.browserCtx)
	defer cancelTab()
	defer context.AfterFunc(ctx, cancelTab)()

	if err := chromedp.Do(tab, chromedp.Navigate(redeemURL)); err != nil {
		return "", fmt.Errorf("%w: load redeem page: %v", ErrNotSubmitted, err)
	}

	// Before submitting: make sure we're logged in and nothing is blocking the form.
	st, err := readPageState(tab)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotSubmitted, err)
	}
	switch st.State {
	case "signin":
		return "", fmt.Errorf("%w: amazon session expired, export fresh cookies", ErrNotSubmitted)
	case "captcha":
		return "", fmt.Errorf("%w: amazon is showing a captcha", ErrNotSubmitted)
	case "error":
		return "", fmt.Errorf("%w: redeem page shows an error before submitting: %s", ErrNotSubmitted, st.Text)
	}

	formCtx, cancelForm := context.WithTimeout(tab, 20*time.Second)
	defer cancelForm()
	if err := chromedp.Do(formCtx,
		chromedp.WaitVisible(codeInputSel),
		chromedp.WaitVisible(applyButtonSel),
		chromedp.SendKeys(codeInputSel, formatCode(code)),
	); err != nil {
		return "", fmt.Errorf("%w: redeem form not found: %v", ErrNotSubmitted, err)
	}

	// From here on the code may have been applied, so unexpected errors are "unknown outcome".
	if err := chromedp.Do(tab, chromedp.Click(applyButtonSel)); err != nil {
		return "", fmt.Errorf("click apply: %w", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		st, err := readPageState(tab)
		if err != nil {
			continue // page is navigating
		}
		switch st.State {
		case "success":
			return st.Text, nil
		case "error":
			return "", fmt.Errorf("%w: %s", ErrCardRejected, st.Text)
		case "signin":
			// Amazon asks for re-authentication before applying the balance.
			return "", fmt.Errorf("%w: amazon asked to sign in again, export fresh cookies", ErrNotSubmitted)
		case "captcha":
			return "", fmt.Errorf("%w: amazon is showing a captcha", ErrNotSubmitted)
		}
	}
	return "", errors.New("no result shown after submitting; check the Amazon balance")
}

func readPageState(tab context.Context) (pageState, error) {
	return chromedp.Run(tab, chromedp.Evaluate[pageState](pageStateJS))
}

// FakeRedeemer is for local testing: codes starting with "BAD" are rejected, others succeed.
type FakeRedeemer struct{}

func (FakeRedeemer) Redeem(ctx context.Context, code string) (string, error) {
	if !sleep(ctx, 2*time.Second) {
		return "", ctx.Err()
	}
	if strings.HasPrefix(code, "BAD") {
		return "", fmt.Errorf("%w: fake rejection", ErrCardRejected)
	}
	return "fake: 10,00 € added to balance", nil
}
