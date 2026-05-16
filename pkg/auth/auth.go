package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	firebase "firebase.google.com/go/v4"
	firebaseauth "firebase.google.com/go/v4/auth"
	"google.golang.org/api/idtoken"
	"google.golang.org/api/option"
)

const FirebaseTokenHeader = "X-Firebase-Token"

type Mode string

const (
	ModeDev        Mode = "dev"
	ModeDevStrict  Mode = "dev_strict"
	ModeProduction Mode = "production"
)

func ParseMode(raw string) Mode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(ModeProduction):
		return ModeProduction
	case string(ModeDevStrict):
		return ModeDevStrict
	default:
		return ModeDev
	}
}

type Config struct {
	Mode                    Mode
	GCPAudience             string
	AllowedCallerIdentities []string
	FirebaseProjectID       string
}

type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	return e.Message
}

type Verifier struct {
	cfg Config

	initOnce sync.Once
	initErr  error
	client   *firebaseauth.Client
}

func NewVerifier(cfg Config) *Verifier {
	return &Verifier{cfg: cfg}
}

func (v *Verifier) ResolveBillingUser(ctx context.Context, r *http.Request, bodyUserID string) (string, *HTTPError) {
	if err := v.verifyCaller(ctx, r.Header.Get("Authorization"), audienceFromRequest(r)); err != nil {
		return "", err
	}

	firebaseUID, err := v.verifyFirebaseUser(ctx, r.Header.Get(FirebaseTokenHeader))
	if err != nil {
		return "", err
	}

	return resolveBillingUserID(bodyUserID, firebaseUID)
}

func (v *Verifier) verifyCaller(ctx context.Context, authHeader, audience string) *HTTPError {
	if !v.isProduction() {
		return nil
	}

	token := parseBearerToken(authHeader)
	if token == "" {
		return &HTTPError{StatusCode: http.StatusUnauthorized, Message: "missing bearer token"}
	}
	verifiedAudience := strings.TrimSpace(audience)
	if verifiedAudience == "" {
		verifiedAudience = strings.TrimSpace(v.cfg.GCPAudience)
	}
	if verifiedAudience == "" {
		return &HTTPError{StatusCode: http.StatusInternalServerError, Message: "GCP audience is not configured"}
	}

	payload, err := idtoken.Validate(ctx, token, verifiedAudience)
	if err != nil {
		return &HTTPError{StatusCode: http.StatusUnauthorized, Message: "invalid bearer token"}
	}

	email, _ := payload.Claims["email"].(string)
	sub, _ := payload.Claims["sub"].(string)
	if len(v.cfg.AllowedCallerIdentities) > 0 && !matchesIdentity(v.cfg.AllowedCallerIdentities, email, sub) {
		return &HTTPError{StatusCode: http.StatusUnauthorized, Message: "unauthorized caller"}
	}
	return nil
}

func (v *Verifier) verifyFirebaseUser(ctx context.Context, firebaseToken string) (string, *HTTPError) {
	firebaseToken = strings.TrimSpace(firebaseToken)
	if !v.requiresFirebaseToken() {
		return "", nil
	}
	if firebaseToken == "" {
		return "", &HTTPError{StatusCode: http.StatusUnauthorized, Message: "missing firebase token"}
	}

	if err := v.ensureFirebaseClient(); err != nil {
		return "", &HTTPError{StatusCode: http.StatusInternalServerError, Message: "firebase auth is not configured"}
	}

	token, err := v.client.VerifyIDToken(ctx, firebaseToken)
	if err != nil {
		return "", &HTTPError{StatusCode: http.StatusUnauthorized, Message: "invalid firebase token"}
	}
	return token.UID, nil
}

func (v *Verifier) ensureFirebaseClient() error {
	v.initOnce.Do(func() {
		initCtx := context.Background()
		baseCfg := &firebase.Config{ProjectID: strings.TrimSpace(v.cfg.FirebaseProjectID)}
		app, err := firebase.NewApp(initCtx, baseCfg)
		if err != nil {
			if v.isProduction() {
				v.initErr = err
				return
			}
			app, err = firebase.NewApp(initCtx, baseCfg, option.WithoutAuthentication())
			if err != nil {
				v.initErr = err
				return
			}
		}
		client, err := app.Auth(initCtx)
		if err != nil {
			v.initErr = err
			return
		}
		v.client = client
	})

	if v.initErr != nil {
		return v.initErr
	}
	if v.client == nil {
		return errors.New("firebase auth client is nil")
	}
	return nil
}

func (v *Verifier) isProduction() bool {
	return v.cfg.Mode == ModeProduction
}

func (v *Verifier) requiresFirebaseToken() bool {
	return v.cfg.Mode == ModeProduction || v.cfg.Mode == ModeDevStrict
}

func audienceFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	host := strings.TrimSpace(r.Host)
	if host == "" {
		return ""
	}
	return "https://" + host
}

func parseBearerToken(authHeader string) string {
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
}

func contains(items []string, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return false
	}
	for _, item := range items {
		if strings.TrimSpace(item) == want {
			return true
		}
	}
	return false
}

func matchesIdentity(allowed []string, email, sub string) bool {
	return contains(allowed, email) || contains(allowed, sub)
}

func resolveBillingUserID(bodyUserID, firebaseUID string) (string, *HTTPError) {
	bodyUserID = strings.TrimSpace(bodyUserID)
	firebaseUID = strings.TrimSpace(firebaseUID)
	if firebaseUID == "" {
		if bodyUserID == "" {
			return "", &HTTPError{StatusCode: http.StatusBadRequest, Message: "user_id is required when firebase token is missing"}
		}
		return bodyUserID, nil
	}
	if bodyUserID != "" && bodyUserID != firebaseUID {
		return "", &HTTPError{StatusCode: http.StatusForbidden, Message: "user_id does not match firebase identity"}
	}
	return firebaseUID, nil
}
