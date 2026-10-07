package awscred

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fakeSecret = "fakeSecretKey/NotReal0123456789"

func fakeSTS(t *testing.T, status int) *url.Values {
	t.Helper()
	got := &url.Values{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		*got = r.PostForm
		if status != 200 {
			w.WriteHeader(status)
			w.Write([]byte(`<ErrorResponse><Error><Code>AccessDenied</Code><Message>no</Message></Error></ErrorResponse>`))
			return
		}
		if r.PostForm.Get("Action") == "GetCallerIdentity" {
			w.Write([]byte(`<GetCallerIdentityResponse><GetCallerIdentityResult><Arn>arn:aws:sts::123456789012:assumed-role/r/s</Arn></GetCallerIdentityResult></GetCallerIdentityResponse>`))
			return
		}
		w.Write([]byte(`<AssumeRoleResponse><AssumeRoleResult><Credentials><AccessKeyId>ASIAFAKE</AccessKeyId><SecretAccessKey>fakeTempSecret</SecretAccessKey><SessionToken>fakeToken</SessionToken><Expiration>2030-01-01T00:00:00Z</Expiration></Credentials></AssumeRoleResult></AssumeRoleResponse>`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ENDPOINT_URL_STS", srv.URL)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", os.DevNull)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", os.DevNull)
	return got
}

var role = Spec{Region: "eu-west-1", RoleARN: "arn:aws:iam::123456789012:role/r", ExternalID: "ext-1",
	AccessKeyID: "AKIAFAKE", SecretAccessKey: fakeSecret}

func TestAssumeRoleForm(t *testing.T) {
	got := fakeSTS(t, 200)
	c, err := Get(context.Background(), role, 30*time.Minute, "siphon-7")
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessKeyID != "ASIAFAKE" || c.SessionToken != "fakeToken" || c.Expires.IsZero() {
		t.Fatalf("creds %+v", c)
	}
	for k, v := range map[string]string{"Action": "AssumeRole", "DurationSeconds": "1800",
		"RoleSessionName": "siphon-7", "ExternalId": "ext-1", "RoleArn": role.RoleARN} {
		if got.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, got.Get(k), v)
		}
	}
}

func TestDuration(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		0: 15 * time.Minute, 10 * time.Minute: 15 * time.Minute,
		50 * time.Minute: 55 * time.Minute, 56 * time.Minute: time.Hour, 2 * time.Hour: time.Hour, 20 * time.Minute: 25 * time.Minute} {
		if got := Duration(in); got != want {
			t.Errorf("Duration(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestStaticProfileRefused(t *testing.T) {
	fakeSTS(t, 200)
	dir := t.TempDir()
	cf := filepath.Join(dir, "config")
	os.WriteFile(cf, []byte("[profile p]\n"), 0o600)
	cr := filepath.Join(dir, "creds")
	os.WriteFile(cr, []byte("[p]\naws_access_key_id=AKIAFAKE\naws_secret_access_key="+fakeSecret+"\n"), 0o600)
	t.Setenv("AWS_CONFIG_FILE", cf)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", cr)
	_, err := Get(context.Background(), Spec{Region: "eu-west-1", Profile: "p"}, time.Hour, "s")
	if err == nil || !strings.Contains(err.Error(), "long-lived") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), fakeSecret) {
		t.Fatal("error leaks secret")
	}
}

func TestIdentity(t *testing.T) {
	fakeSTS(t, 200)
	arn, err := Identity(context.Background(), role, Creds{AccessKeyID: "ASIAFAKE", SecretAccessKey: "x", SessionToken: "t"})
	if err != nil || arn != "arn:aws:sts::123456789012:assumed-role/r/s" {
		t.Fatalf("arn %q err %v", arn, err)
	}
}

func TestErrorsHideSecret(t *testing.T) {
	fakeSTS(t, 403)
	_, err := Get(context.Background(), role, time.Hour, "s")
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), fakeSecret) {
		t.Fatal("error leaks secret")
	}
	if got := scrub(errString("boom "+fakeSecret), fakeSecret); strings.Contains(got.Error(), fakeSecret) {
		t.Fatal("scrub failed")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestRefusesKeysThatExpireTooSoon(t *testing.T) {
	fakeSTS(t, 200) // keys expire 2030-01-01T00:00:00Z
	old := now
	t.Cleanup(func() { now = old })
	exp := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	now = func() time.Time { return exp.Add(-20 * time.Minute) } // 30 min run needs 25 min
	if _, err := Get(context.Background(), role, 30*time.Minute, "s"); err == nil || !strings.Contains(err.Error(), "before this run could finish") {
		t.Fatalf("err = %v", err)
	}
	now = func() time.Time { return exp.Add(-26 * time.Minute) }
	if _, err := Get(context.Background(), role, 30*time.Minute, "s"); err != nil {
		t.Fatal(err)
	}
}
