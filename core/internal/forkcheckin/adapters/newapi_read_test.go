package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
)

func readFixture(t *testing.T, dialect string, handler http.HandlerFunc) (*NewAPIRead, forkcheckin.AccountSnapshot) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	factory := forkcheckin.NewTransportFactory()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := factory.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	adapter, err := NewNewAPIRead(factory, dialect)
	if err != nil {
		t.Fatal(err)
	}
	credential := forkcheckin.NetworkCredential{Version: 1, Bearer: "dashboard-session-canary"}
	if dialect == NewAPILegacy {
		credential.Bearer = ""
		credential.Cookies = []forkcheckin.SessionCookie{{Name: "session", Value: "legacy-cookie-canary", Path: "/", HTTPOnly: true}}
	}
	encoded, err := forkcheckin.EncodeNetworkCredential(credential)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clear(encoded) })
	return adapter, forkcheckin.AccountSnapshot{Account: forkcheckin.Account{
		ID: "account_one", DashboardBaseURL: server.URL + "/mounted%20app/", State: forkcheckin.AccountStateConnected,
		Revision: 1, Network: forkcheckin.Network{Mode: forkcheckin.NetworkModeDirect}, TimeZone: "Pacific/Kiritimati", RemoteUserID: "7",
	}, Credential: encoded}
}

func reply(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = io.WriteString(w, body)
}

func TestNewAPIReadOnlyDialectsAndSubpaths(t *testing.T) {
	for _, dialect := range []string{NewAPILegacy, NewAPIModern} {
		t.Run(dialect, func(t *testing.T) {
			var paths []string
			var mu sync.Mutex
			adapter, snapshot := readFixture(t, dialect, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.EscapedPath())
				mu.Unlock()
				if r.Method != http.MethodGet || r.URL.RawQuery != "" {
					t.Errorf("unexpected method/query: %s %s", r.Method, r.URL.RawQuery)
				}
				if body, _ := io.ReadAll(r.Body); len(body) != 0 {
					t.Error("read sent a body")
				}
				if dialect == NewAPILegacy {
					if r.Header.Get("New-Api-User") != "7" || r.Header.Get("Cookie") != "session=legacy-cookie-canary" || r.Header.Get("Authorization") != "" {
						t.Error("legacy auth mixed with modern")
					}
				} else if r.Header.Get("Authorization") != "Bearer dashboard-session-canary" || r.Header.Get("Cookie") != "" || r.Header.Get("New-Api-User") != "" {
					t.Error("modern auth mixed with legacy")
				}
				for key, values := range r.Header {
					if strings.HasPrefix(strings.ToLower(key), "x-astrlink-") || strings.Contains(strings.ToLower(strings.Join(values, "")), "astrlink") {
						t.Error("gateway identity reached upstream")
					}
				}
				switch r.URL.Path {
				case "/mounted app/api/status":
					reply(w, `{"success":true,"data":{"checkin_enabled":true,"turnstile_check":false,"server_address":"https://other.example"}}`)
				case "/mounted app/api/user/self":
					reply(w, `{"success":true,"data":{"id":7,"username":"private-user-canary","email":"private-email-canary","quota":999999999,"access_token":"private-token-canary"}}`)
				case "/mounted app/api/user/checkin":
					reply(w, `{"success":true,"data":{"enabled":true,"stats":{"checked_in_today":true,"records":[{"checkin_date":"2024-02-29","quota_awarded":0}]}}}`)
				default:
					t.Errorf("unapproved endpoint: %s", r.URL.Path)
					w.WriteHeader(404)
				}
			})
			before := snapshot.Account
			capability, err := adapter.Inspect(context.Background(), snapshot)
			if err != nil || !capability.Supported || capability.RequiresManual || capability.Dialect != dialect {
				t.Fatalf("capability: %+v %v", capability, err)
			}
			identity, err := adapter.ValidateIdentity(context.Background(), snapshot)
			if err != nil || identity.RemoteUserID != "7" || identity.DisplayHint != "" {
				t.Fatalf("identity: %+v %v", identity, err)
			}
			status, err := adapter.ReadStatus(context.Background(), snapshot)
			if err != nil || !status.CheckedInToday || status.SiteDate != "" || status.Reward != nil || status.NextAvailableAt != nil {
				t.Fatalf("status: %+v %v", status, err)
			}
			if len(status.Records) != 1 || status.Records[0].SiteDate != "2024-02-29" || !status.Records[0].Reward.Known || status.Records[0].Reward.Unit != "quota" {
				t.Fatalf("historical reward lost: %+v", status)
			}
			if !reflect.DeepEqual(snapshot.Account, before) {
				t.Error("read mutated account")
			}
			for _, value := range []any{capability, identity, status} {
				if strings.Contains(fmt.Sprintf("%+v", value), "canary") {
					t.Error("private site data published")
				}
			}
			mu.Lock()
			defer mu.Unlock()
			want := []string{"/mounted%20app/api/status", "/mounted%20app/api/user/self", "/mounted%20app/api/user/self", "/mounted%20app/api/user/checkin"}
			if !reflect.DeepEqual(paths, want) {
				t.Fatalf("requests=%v want=%v", paths, want)
			}
		})
	}
}

func TestNewAPIIdentityMismatchStopsBeforeStatus(t *testing.T) {
	for _, dialect := range []string{NewAPILegacy, NewAPIModern} {
		t.Run(dialect, func(t *testing.T) {
			adapter, snapshot := readFixture(t, dialect, func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/self") {
					t.Error("status reached with wrong identity")
				}
				reply(w, `{"success":true,"data":{"id":8}}`)
			})
			status, err := adapter.ReadStatus(context.Background(), snapshot)
			if !errors.Is(err, forkcheckin.ErrIdentityMismatch) || !reflect.DeepEqual(status, forkcheckin.CheckInStatus{}) {
				t.Fatalf("wrong identity: %+v %v", status, err)
			}
		})
	}
}

func TestNewAPIDraftAndUIDValidationDoNotGuess(t *testing.T) {
	for _, dialect := range []string{NewAPILegacy, NewAPIModern} {
		t.Run(dialect, func(t *testing.T) {
			adapter, snapshot := readFixture(t, dialect, func(w http.ResponseWriter, r *http.Request) {
				if dialect != NewAPIModern || !strings.HasSuffix(r.URL.Path, "/self") {
					t.Error("unsupported draft performed network request")
				}
				reply(w, `{"success":true,"data":{"id":9007199254740993}}`)
			})
			snapshot.Account.State, snapshot.Account.RemoteUserID = forkcheckin.AccountStateDraft, ""
			identity, err := adapter.ValidateIdentity(context.Background(), snapshot)
			if dialect == NewAPIModern {
				if err != nil || identity.RemoteUserID != "9007199254740993" {
					t.Fatalf("UID rounded or not read from self: %v %v", identity, err)
				}
			} else if !errors.Is(err, forkcheckin.ErrManualRequired) {
				t.Fatalf("legacy guessed UID: %v", err)
			}
			if _, err := adapter.ReadStatus(context.Background(), snapshot); err == nil {
				t.Error("draft consumed account status")
			}
			snapshot.Account.State = forkcheckin.AccountStateConnected
			for _, id := range []string{"0", "-1", "07", "7.0", "7e0", "+7", "7\r\nheader", "9223372036854775808"} {
				snapshot.Account.RemoteUserID = id
				if _, err := adapter.ValidateIdentity(context.Background(), snapshot); !errors.Is(err, forkcheckin.ErrIdentityMismatch) {
					t.Errorf("UID %q: %v", id, err)
				}
			}
		})
	}
}

func TestNewAPIReadFailureClassification(t *testing.T) {
	cases := []struct {
		name              string
		code              int
		contentType, body string
		want              error
	}{
		{"disabled", 200, "application/json", `{"success":true,"data":{"checkin_enabled":false}}`, forkcheckin.ErrCheckInDisabled},
		{"missing endpoint", 404, "text/html", `<html>canary</html>`, forkcheckin.ErrUnsupported},
		{"auth", 401, "application/json", `{"success":false,"message":"secret-canary"}`, forkcheckin.ErrAuthRequired},
		{"permission", 403, "application/json", `{"success":false,"message":"secret-canary"}`, forkcheckin.ErrPermissionDenied},
		{"HTML verification", 403, "text/html", `<html>challenge secret-canary</html>`, forkcheckin.ErrManualRequired},
		{"HTML disguised", 200, "application/json", `<html>challenge</html>`, forkcheckin.ErrManualRequired},
		{"rate limit", 429, "text/html", `<html>canary</html>`, forkcheckin.ErrRateLimited},
		{"server failure", 503, "application/json", `{}`, forkcheckin.ErrNetwork},
		{"old disabled", 200, "application/json", `{"success":false,"message":"签到功能未启用"}`, forkcheckin.ErrCheckInDisabled},
		{"legacy token", 200, "application/json", `{"success":false,"message":"无权进行此操作，access token 无效"}`, forkcheckin.ErrAuthRequired},
		{"legacy permission", 200, "application/json", `{"success":false,"message":"无权进行此操作，权限不足"}`, forkcheckin.ErrPermissionDenied},
		{"modern expired", 200, "application/json", `{"success":false,"code":"AUTH_TOKEN_EXPIRED","message":"secret-canary"}`, forkcheckin.ErrAuthRequired},
		{"unknown dialect", 200, "application/json", `{"success":false,"message":"secret-canary"}`, forkcheckin.ErrUnsupported},
		{"missing success", 200, "application/json", `{"data":{"checkin_enabled":true}}`, forkcheckin.ErrUnsupported},
		{"missing enabled", 200, "application/json", `{"success":true,"data":{}}`, forkcheckin.ErrUnsupported},
		{"null enabled", 200, "application/json", `{"success":true,"data":{"checkin_enabled":null}}`, forkcheckin.ErrUnsupported},
		{"wrong enabled", 200, "application/json", `{"success":true,"data":{"checkin_enabled":"true"}}`, forkcheckin.ErrUnsupported},
		{"duplicate success", 200, "application/json", `{"success":false,"Success":true,"data":{"checkin_enabled":true}}`, forkcheckin.ErrUnsupported},
		{"duplicate data key", 200, "application/json", `{"success":true,"data":{"checkin_enabled":false,"CHECKIN_ENABLED":true}}`, forkcheckin.ErrUnsupported},
		{"trailing document", 200, "application/json", `{"success":true,"data":{"checkin_enabled":true}}{}`, forkcheckin.ErrUnsupported},
		{"null data", 200, "application/json", `{"success":true,"data":null}`, forkcheckin.ErrUnsupported},
		{"wrong media", 200, "text/plain", `{"success":true,"data":{"checkin_enabled":true}}`, forkcheckin.ErrUnsupported},
		{"oversize", 200, "application/json", strings.Repeat("x", maxReadBytes+1), forkcheckin.ErrResponseTooLarge},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			adapter, snapshot := readFixture(t, NewAPIModern, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("read performed write")
				}
				w.Header().Set("Content-Type", test.contentType)
				w.WriteHeader(test.code)
				_, _ = io.WriteString(w, test.body)
			})
			capability, err := adapter.Inspect(context.Background(), snapshot)
			if !errors.Is(err, test.want) {
				t.Fatalf("err=%v want=%v", err, test.want)
			}
			if strings.Contains(err.Error(), "canary") || capability.Supported {
				t.Error("site failure leaked or established support")
			}
			if capability.RequiresManual != errors.Is(test.want, forkcheckin.ErrManualRequired) {
				t.Error("manual-required flag not classified")
			}
		})
	}
}

func TestNewAPIReadRejectsRedirectEvenToSameOrigin(t *testing.T) {
	adapter, snapshot := readFixture(t, NewAPIModern, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mounted app/api/status" {
			t.Error("redirect reached potentially state-changing GET")
		}
		w.Header().Set("Location", "/mounted%20app/api/user/logout")
		w.WriteHeader(http.StatusFound)
	})
	if _, err := adapter.Inspect(context.Background(), snapshot); !errors.Is(err, forkcheckin.ErrNetworkRedirect) {
		t.Fatalf("redirect not refused: %v", err)
	}
}

func TestNewAPIStatusRequiresExplicitTodayAndValidRecords(t *testing.T) {
	for _, test := range []struct {
		name, data string
		want       error
	}{
		{"disabled", `{"enabled":false}`, forkcheckin.ErrCheckInDisabled},
		{"no enabled", `{"stats":{"checked_in_today":true}}`, forkcheckin.ErrUnsupported},
		{"no stats", `{"enabled":true}`, forkcheckin.ErrUnsupported},
		{"balance only", `{"enabled":true,"stats":{"total_quota":9999,"records":[]}}`, forkcheckin.ErrUnsupported},
		{"null today", `{"enabled":true,"stats":{"checked_in_today":null}}`, forkcheckin.ErrUnsupported},
		{"duplicate today", `{"enabled":true,"stats":{"checked_in_today":false,"checked_in_today":true}}`, forkcheckin.ErrUnsupported},
		{"bad date", `{"enabled":true,"stats":{"checked_in_today":true,"records":[{"checkin_date":"2023-02-29","quota_awarded":1}]}}`, forkcheckin.ErrUnsupported},
		{"missing reward", `{"enabled":true,"stats":{"checked_in_today":true,"records":[{"checkin_date":"2024-02-29"}]}}`, forkcheckin.ErrUnsupported},
		{"negative reward", `{"enabled":true,"stats":{"checked_in_today":true,"records":[{"checkin_date":"2024-02-29","quota_awarded":-1}]}}`, forkcheckin.ErrUnsupported},
		{"duplicate date", `{"enabled":true,"stats":{"checked_in_today":true,"records":[{"checkin_date":"2024-02-29","quota_awarded":1},{"checkin_date":"2024-02-29","quota_awarded":2}]}}`, forkcheckin.ErrUnsupported},
		{"null record", `{"enabled":true,"stats":{"checked_in_today":true,"records":[null]}}`, forkcheckin.ErrUnsupported},
		{"empty records", `{"enabled":true,"stats":{"checked_in_today":true,"records":[]}}`, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter, snapshot := readFixture(t, NewAPIModern, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/self") {
					reply(w, `{"success":true,"data":{"id":7}}`)
					return
				}
				reply(w, `{"success":true,"data":`+test.data+`}`)
			})
			result, err := adapter.ReadStatus(context.Background(), snapshot)
			if !errors.Is(err, test.want) {
				t.Fatalf("%+v %v want %v", result, err, test.want)
			}
			if test.want != nil && !reflect.DeepEqual(result, forkcheckin.CheckInStatus{}) {
				t.Error("failed parse returned a partial status")
			}
		})
	}
}

func TestNewAPIServerMonthBoundaryNeverInventsTodayDate(t *testing.T) {
	for _, checked := range []bool{false, true} {
		for _, zone := range []string{"Pacific/Kiritimati", "Pacific/Pago_Pago"} {
			t.Run(fmt.Sprintf("%t_%s", checked, zone), func(t *testing.T) {
				adapter, snapshot := readFixture(t, NewAPIModern, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.RawQuery != "" {
						t.Error("client supplied its own month")
					}
					if strings.HasSuffix(r.URL.Path, "/self") {
						reply(w, `{"success":true,"data":{"id":7}}`)
						return
					}
					w.Header().Set("Date", time.Date(2025, 1, 1, 0, 5, 0, 0, time.UTC).Format(http.TimeFormat))
					reply(w, fmt.Sprintf(`{"success":true,"data":{"enabled":true,"stats":{"checked_in_today":%t,"total_quota":9999,"records":[{"checkin_date":"2024-12-31","quota_awarded":17}]}}}`, checked))
				})
				snapshot.Account.TimeZone = zone
				status, err := adapter.ReadStatus(context.Background(), snapshot)
				if err != nil || status.CheckedInToday != checked || status.SiteDate != "" || status.Reward != nil {
					t.Fatalf("inferred today's date or reward: %+v %v", status, err)
				}
				if status.Records[0].SiteDate != "2024-12-31" || status.Records[0].Reward.Quota != 17 {
					t.Error("server date was changed")
				}
			})
		}
	}
}

func TestNewAPISelfRejectsMalformedUID(t *testing.T) {
	for _, id := range []string{`null`, `0`, `-1`, `7.0`, `7e0`, `"7"`, `true`, `9223372036854775808`, `{}`} {
		t.Run(id, func(t *testing.T) {
			adapter, snapshot := readFixture(t, NewAPIModern, func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/self") {
					t.Error("malformed self reached status")
				}
				reply(w, `{"success":true,"data":{"id":`+id+`}}`)
			})
			identity, err := adapter.ValidateIdentity(context.Background(), snapshot)
			if !errors.Is(err, forkcheckin.ErrUnsupported) || identity.RemoteUserID != "" {
				t.Fatalf("invalid self accepted: %+v %v", identity, err)
			}
		})
	}
}

func TestNewAPIRecordCountIsBounded(t *testing.T) {
	adapter, snapshot := readFixture(t, NewAPIModern, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/self") {
			reply(w, `{"success":true,"data":{"id":7}}`)
			return
		}
		records := strings.TrimSuffix(strings.Repeat(`{"checkin_date":"2000-01-01","quota_awarded":1},`, 32), ",")
		reply(w, `{"success":true,"data":{"enabled":true,"stats":{"checked_in_today":true,"records":[`+records+`]}}}`)
	})
	if _, err := adapter.ReadStatus(context.Background(), snapshot); !errors.Is(err, forkcheckin.ErrUnsupported) {
		t.Fatalf("unbounded records: %v", err)
	}
}

func TestNewAPIManualCapabilityAndNoSubmitSurface(t *testing.T) {
	adapter, snapshot := readFixture(t, NewAPIModern, func(w http.ResponseWriter, _ *http.Request) {
		reply(w, `{"success":true,"data":{"checkin_enabled":true,"turnstile_check":true}}`)
	})
	capability, err := adapter.Inspect(context.Background(), snapshot)
	if err != nil || !capability.Supported || !capability.RequiresManual {
		t.Fatalf("manual capability: %+v %v", capability, err)
	}
	if _, ok := any(adapter).(forkcheckin.SiteAdapter); ok {
		t.Error("read-only adapter implements Submit")
	}
	if _, err := NewNewAPIRead(nil, NewAPIModern); !errors.Is(err, forkcheckin.ErrUnsupported) {
		t.Error("nil factory accepted")
	}
	if _, err := NewNewAPIRead(adapter.factory, "auto"); !errors.Is(err, forkcheckin.ErrUnsupported) {
		t.Error("automatic dialect fallback accepted")
	}
}

func TestNewAPIReadHonorsCancellationAndFactoryInvalidation(t *testing.T) {
	adapter, snapshot := readFixture(t, NewAPIModern, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := adapter.Inspect(ctx, snapshot); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request not cancelled: %v", err)
	}
	if err := adapter.factory.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Inspect(context.Background(), snapshot); !errors.Is(err, forkcheckin.ErrClientInvalidated) {
		t.Fatalf("closed factory used: %v", err)
	}
}

func TestNewAPIUnambiguousJSONLimits(t *testing.T) {
	for _, body := range []string{"{\"text\":\"\xff\"}", `{"x":1,"X":2}`, `{} {}`, `{"data":{"id":7,"id":8}}`, strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34), `{"data":`} {
		if unambiguousJSON([]byte(body)) {
			t.Errorf("ambiguous JSON accepted: %q", body)
		}
	}
	var data any
	body := `{"success":true,"data":{"checked_in_today":false,"other":[null,true,1]}}`
	if json.Unmarshal([]byte(body), &data) != nil || !unambiguousJSON([]byte(body)) {
		t.Error("ordinary extension fields rejected")
	}
}
