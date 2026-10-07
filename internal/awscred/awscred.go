// Package awscred fetches short-lived AWS credentials for a run.
package awscred

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Spec mirrors a provider: aws credential, secrets already resolved.
// Exactly one of Profile or RoleARN is set; the base keys need RoleARN.
type Spec struct {
	Region, Profile, RoleARN, ExternalID string
	AccessKeyID, SecretAccessKey         string
}

type Creds struct {
	AccessKeyID, SecretAccessKey, SessionToken string
	Expires                                    time.Time
}

var now = time.Now // tests replace it

// Duration is the session length for an agent timeout: +5 min, 15 min..1 h.
func Duration(timeout time.Duration) time.Duration {
	return min(max(timeout+5*time.Minute, 15*time.Minute), time.Hour)
}

func load(ctx context.Context, s Spec, opts ...func(*config.LoadOptions) error) (aws.Config, error) {
	opts = append(opts, config.WithRegion(s.Region))
	if s.Profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(s.Profile))
	}
	if s.AccessKeyID != "" {
		opts = append(opts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(s.AccessKeyID, s.SecretAccessKey, "")))
	}
	return config.LoadDefaultConfig(ctx, opts...)
}

// Get returns credentials valid for d. session names the assumed-role session.
func Get(ctx context.Context, s Spec, d time.Duration, session string) (Creds, error) {
	c, err := get(ctx, s, d, session)
	return c, scrub(err, s.SecretAccessKey)
}

func get(ctx context.Context, s Spec, d time.Duration, session string) (Creds, error) {
	// A profile that assumes a role (role_arn + source_profile) gets the
	// run's duration and session name too; SSO sets its own lifetime.
	cfg, err := load(ctx, s, config.WithAssumeRoleCredentialOptions(func(o *stscreds.AssumeRoleOptions) {
		o.Duration = d
		o.RoleSessionName = session
	}))
	if err != nil {
		return Creds{}, err
	}
	p := cfg.Credentials
	if s.RoleARN != "" {
		p = stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), s.RoleARN, func(o *stscreds.AssumeRoleOptions) {
			o.Duration = d
			o.RoleSessionName = session
			if s.ExternalID != "" {
				o.ExternalID = aws.String(s.ExternalID)
			}
		})
	}
	v, err := p.Retrieve(ctx)
	if err != nil {
		return Creds{}, err
	}
	if s.RoleARN == "" && !v.CanExpire {
		return Creds{}, errors.New("profile gives long-lived keys; use SSO, a role, or credential_process")
	}
	// The run needs d minus the 5 min margin Duration added.
	if v.CanExpire && v.Expires.Before(now().Add(d-5*time.Minute)) {
		return Creds{}, fmt.Errorf("credentials expire at %s, before this run could finish; refresh the SSO login or credential_process", v.Expires.UTC().Format(time.RFC3339))
	}
	return Creds{v.AccessKeyID, v.SecretAccessKey, v.SessionToken, v.Expires}, nil
}

// Identity returns the caller ARN for c.
func Identity(ctx context.Context, s Spec, c Creds) (string, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(s.Region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			c.AccessKeyID, c.SecretAccessKey, c.SessionToken)))
	if err != nil {
		return "", scrub(err, c.SecretAccessKey)
	}
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", scrub(err, c.SecretAccessKey, s.SecretAccessKey)
	}
	return aws.ToString(out.Arn), nil
}

// scrub drops secrets from an error message, in case the SDK echoes one.
func scrub(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	m := err.Error()
	for _, v := range secrets {
		if v != "" {
			m = strings.ReplaceAll(m, v, "***")
		}
	}
	return errors.New(m)
}
