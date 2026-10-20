package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/descope/virtualwebauthn"
	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
)

func TestPasskeyEndpoints(t *testing.T) {
	srv, _ := newAuthServer(t)
	_, body := do(t, "POST", srv.URL+"/v1/auth/signup", "", `{"email":"a@example.com","password":"a long passphrase","name":"A"}`)
	tok := body["session_token"].(string)
	user := body["user"].(map[string]any)
	uid := uuid.MustParse(user["id"].(string))

	// Left pending: confirming it must fail once the account has a passkey.
	_, pending := do(t, "POST", srv.URL+"/v1/auth/totp", tok, `{"password":"a long passphrase"}`)

	// A password session must re-authenticate to add a passkey.
	if st, b := do(t, "POST", srv.URL+"/v1/auth/passkeys/register", tok, ""); st != 401 || errCode(b) != "password_required" {
		t.Fatalf("no password: %d %v", st, b)
	}
	if st, b := do(t, "POST", srv.URL+"/v1/auth/passkeys/register", tok, `{"password":"wrong password!"}`); st != 401 || errCode(b) != "bad_credentials" {
		t.Fatalf("wrong password: %d %v", st, b)
	}
	st, ch := do(t, "POST", srv.URL+"/v1/auth/passkeys/register", tok, `{"password":"a long passphrase"}`)
	if st != 200 {
		t.Fatalf("begin register: %d %v", st, ch)
	}

	rp := virtualwebauthn.RelyingParty{Name: "TicketSlinger", ID: "localhost", Origin: "http://localhost:5173"}
	authr := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{UserHandle: uid[:]})
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	opts, _ := json.Marshal(ch["options"])
	att, err := virtualwebauthn.ParseAttestationOptions(string(opts))
	if err != nil {
		t.Fatal(err)
	}
	attResp := virtualwebauthn.CreateAttestationResponse(rp, authr, cred, *att)
	st, pk := do(t, "POST", srv.URL+"/v1/auth/passkeys/register/"+ch["challenge_id"].(string)+"?name="+url.QueryEscape("Work laptop"), tok, attResp)
	if st != 201 || pk["name"] != "Work laptop" {
		t.Fatalf("finish register: %d %v", st, pk)
	}

	// The list shows this account's passkey, and nothing of anyone else's.
	if st, _ := do(t, "GET", srv.URL+"/v1/auth/passkeys", "", ""); st != 401 {
		t.Fatalf("list without a session: %d", st)
	}
	st, list := doList(t, clientFor(t, srv.URL), srv.URL+"/v1/auth/passkeys", tok)
	if st != 200 || len(list) != 1 || list[0]["name"] != "Work laptop" || list[0]["id"] != pk["id"] || list[0]["last_used_at"] != nil || list[0]["credential"] != nil {
		t.Fatalf("list: %d %v", st, list)
	}
	_, other := do(t, "POST", srv.URL+"/v1/auth/signup", "", `{"email":"b@example.com","password":"a long passphrase","name":"B"}`)
	if st, list := doList(t, clientFor(t, srv.URL), srv.URL+"/v1/auth/passkeys", other["session_token"].(string)); st != 200 || len(list) != 0 {
		t.Fatalf("stranger's list: %d %v", st, list)
	}

	st, ch = do(t, "POST", srv.URL+"/v1/auth/passkeys/login", "", "")
	if st != 200 {
		t.Fatalf("begin login: %d %v", st, ch)
	}
	opts, _ = json.Marshal(ch["options"])
	as, err := virtualwebauthn.ParseAssertionOptions(string(opts))
	if err != nil {
		t.Fatal(err)
	}
	asr := virtualwebauthn.CreateAssertionResponse(rp, authr, cred, *as)
	st, sess := do(t, "POST", srv.URL+"/v1/auth/passkeys/login/"+ch["challenge_id"].(string), "", asr)
	if u, _ := sess["user"].(map[string]any); st != 200 || u["mfa"] != true || u["id"] != uid.String() {
		t.Fatalf("finish login: %d %v", st, sess)
	}
	if st, b := do(t, "POST", srv.URL+"/v1/auth/passkeys/login/"+ch["challenge_id"].(string), "", asr); st != 400 || errCode(b) != "challenge_expired" {
		t.Fatalf("replayed login: %d %v", st, b)
	}

	// The passkey session is mfa, so it can add another passkey without a password.
	if st, b := do(t, "GET", srv.URL+"/v1/me", sess["session_token"].(string), ""); st != 200 || b["mfa"] != true {
		t.Fatalf("me with passkey session: %d %v", st, b)
	}
	if st, b := do(t, "POST", srv.URL+"/v1/auth/passkeys/register", sess["session_token"].(string), ""); st != 200 {
		t.Fatalf("mfa session begin register: %d %v", st, b)
	}

	// Now the account has a passkey, the password session can't add a second
	// factor of any kind, even with the right password.
	if st, b := do(t, "POST", srv.URL+"/v1/auth/passkeys/register", tok, `{"password":"a long passphrase"}`); st != 403 || errCode(b) != "mfa_required" {
		t.Fatalf("password session adding a second passkey: %d %v", st, b)
	}
	if st, b := do(t, "POST", srv.URL+"/v1/auth/totp", tok, `{"password":"a long passphrase"}`); st != 403 || errCode(b) != "mfa_required" {
		t.Fatalf("password session starting totp: %d %v", st, b)
	}
	code, _ := totp.GenerateCode(pending["secret"].(string), time.Now())
	if st, b := do(t, "POST", srv.URL+"/v1/auth/totp/confirm", tok, `{"code":"`+code+`"}`); st != 403 || errCode(b) != "mfa_required" {
		t.Fatalf("password session confirming a stale totp: %d %v", st, b)
	}
}

// The passkey sign-in routes are public: no token must not mean 401.
func TestPublicPasskeyLoginNeedsNoSession(t *testing.T) {
	srv, _ := newAuthServer(t)
	st, b := do(t, "POST", srv.URL+"/v1/auth/passkeys/login/0197a2b4-0000-7000-8000-000000000000", "", `{}`)
	if st == 401 || st != 400 || errCode(b) != "challenge_expired" {
		t.Fatalf("finish login without a token: %d %v", st, b)
	}
	if st, b := do(t, "POST", srv.URL+"/v1/auth/passkeys/login", "", ""); st != 200 {
		t.Fatalf("begin login without a token: %d %v", st, b)
	}
}

func doList(t *testing.T, c *http.Client, url, tok string) (int, []map[string]any) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var out []map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}
