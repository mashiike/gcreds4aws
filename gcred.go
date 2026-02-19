package gcreds4aws

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"google.golang.org/api/option"
)

var DefaultCredentialsManager = &CredentialsManager{}

func WithCredentials(ctx context.Context) option.ClientOption {
	return DefaultCredentialsManager.WithCredentials(ctx)
}

func NewCredentials(ctx context.Context) (option.ClientOption, error) {
	return DefaultCredentialsManager.NewCredentialsOption(ctx)
}

func SetSSMClient(client GetParameterAPIClient) {
	DefaultCredentialsManager.SetSSMClient(client)
}

func SetLogger(logger *slog.Logger) {
	DefaultCredentialsManager.SetLogger(logger)
}

func Close() error {
	return DefaultCredentialsManager.Close()
}

type CredentialsManager struct {
	logger                    *slog.Logger
	mu                        sync.Mutex
	awsCfg                    *aws.Config
	ssmClient                 GetParameterAPIClient
	cacheCredentialsExpiresAt time.Time
	cacheCredentialsJSON      []byte
	cacheCredentials          *credentials
	proxyServer               *http.Server
	proxyListener             net.Listener
	proxyRegion               string
	proxyWaitGroup            sync.WaitGroup
}

const (
	CacheLifetimeSeconds                       = 4 * 60
	ServiceAccountImpersonationLifetimeSeconds = 5 * 60
	SubjectTokenTypeForAWS                     = "urn:ietf:params:aws:token-type:aws4_request"
)

func (mgr *CredentialsManager) Close() error {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.proxyServer == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := mgr.proxyServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("failed to shutdown proxy server: %w", err)
	}
	mgr.proxyServer = nil
	if err := mgr.proxyListener.Close(); err != nil {
		mgr.proxyListener = nil
		return fmt.Errorf("failed to close proxy listener: %w", err)
	}
	mgr.proxyListener = nil
	mgr.proxyWaitGroup.Wait()
	mgr.cacheCredentialsExpiresAt = time.Time{}
	mgr.cacheCredentialsJSON = nil
	mgr.cacheCredentials = nil
	return nil
}

type GetParameterAPIClient interface {
	GetParameter(ctx context.Context, input *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

func (mgr *CredentialsManager) SetSSMClient(client GetParameterAPIClient) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.ssmClient = client
}

func (mgr *CredentialsManager) SetLogger(logger *slog.Logger) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.logger = logger
}

func (mgr *CredentialsManager) getLogger() *slog.Logger {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.logger == nil {
		mgr.logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	}
	return mgr.logger.With("module", "gcred4aws")
}

func (mgr *CredentialsManager) WithCredentials(ctx context.Context) option.ClientOption {
	opt, err := mgr.NewCredentialsOption(ctx)
	if err != nil {
		panic(err)
	}
	return opt
}

func (mgr *CredentialsManager) NewCredentialsOption(ctx context.Context) (option.ClientOption, error) {
	logger := mgr.getLogger()
	if opt, ok := mgr.newCredentialsOptionFromCache(); ok {
		logger.DebugContext(ctx, "use cached credentials")
		return opt, nil
	}
	if path := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); path != "" {
		return mgr.newCredentialsOptionFromPath(ctx, path)
	}
	projectNumberStr := os.Getenv("GOOGLE_CLOUD_PROJECT_NUMBER")
	poolID := os.Getenv("GOOGLE_CLOUD_POOL_ID")
	providerID := os.Getenv("GOOGLE_CLOUD_PROVIDER_ID")
	serviceAccountEmail := os.Getenv("GOOGLE_CLOUD_SERVICE_ACCOUNT_EMAIL")
	if projectNumberStr == "" || poolID == "" || providerID == "" || serviceAccountEmail == "" {
		return nil, errors.New("GOOGLE_APPLICATION_CREDENTIALS or Workload Identity Environment Variables(GOOGLE_CLOUD_PROJECT_NUMBER, GOOGLE_CLOUD_POOL_ID, GOOGLE_CLOUD_PROVIDER_ID, GOOGLE_CLOUD_SERVICE_ACCOUNT_EMAIL) is required")
	}

	projectNumber, err := strconv.Atoi(projectNumberStr)
	if err != nil {
		return nil, fmt.Errorf("failed to convert GOOGLE_CLOUD_PROJECT_NUMBER to int: %w", err)
	}

	cred := &credentials{
		Type:                           "external_account",
		Audience:                       fmt.Sprintf("//iam.googleapis.com/projects/%d/locations/global/workloadIdentityPools/%s/providers/%s", projectNumber, poolID, providerID),
		SubjectTokenType:               SubjectTokenTypeForAWS,
		ServiceAccountImpersonationURL: fmt.Sprintf("https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/%s:generateAccessToken", serviceAccountEmail),
		TokenURL:                       "https://sts.googleapis.com/v1/token",
	}
	bs, err := json.Marshal(cred)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal credentials: %w", err)
	}
	return mgr.newCredentialsOptionFromBytes(ctx, bs)
}

func (mgr *CredentialsManager) newCredentialsOptionFromPath(ctx context.Context, path string) (option.ClientOption, error) {
	if strings.HasPrefix(path, "arn:") {
		return mgr.newCredentialsOptionFromArn(ctx, path)
	}
	bs, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read credentials file: %w", err)
	}
	return mgr.newCredentialsOptionFromBytes(ctx, bs)
}

func (mgr *CredentialsManager) newCredentialsOptionFromArn(ctx context.Context, rawARN string) (option.ClientOption, error) {
	arnObj, err := arn.Parse(rawARN)
	if err != nil {
		return nil, fmt.Errorf("failed to parse ARN: %w", err)
	}
	switch arnObj.Service {
	case "ssm":
		return mgr.newCredentialsOptionFromSSM(ctx, arnObj)
	default:
		return nil, fmt.Errorf("unsupported service: %s", arnObj.Service)
	}
}

func (mgr *CredentialsManager) loadConfig(ctx context.Context) (aws.Config, error) {

	if mgr.awsCfg != nil {
		return *mgr.awsCfg, nil
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return aws.Config{}, fmt.Errorf("failed to load AWS config: %w", err)
	}
	mgr.awsCfg = &cfg
	return cfg, nil
}

func (mgr *CredentialsManager) getSSMClient(ctx context.Context) (GetParameterAPIClient, error) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.ssmClient != nil {
		return mgr.ssmClient, nil
	}
	cfg, err := mgr.loadConfig(ctx)
	if err != nil {
		return nil, err
	}
	client := ssm.NewFromConfig(cfg)
	mgr.ssmClient = client
	return client, nil
}

func (mgr *CredentialsManager) newCredentialsOptionFromCache() (option.ClientOption, bool) {
	if bs, cred, ok := mgr.getCachedCredentials(); ok {
		return option.WithAuthCredentialsJSON(cred.credentialsType(), bs), true
	}
	return nil, false
}

func (mgr *CredentialsManager) newCredentialsOptionFromSSM(ctx context.Context, arnObj arn.ARN) (option.ClientOption, error) {
	client, err := mgr.getSSMClient(ctx)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(arnObj.Resource, "/")
	if len(parts) > 1 && parts[0] == "parameter" {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return nil, errors.New("invalid ARN: resource is empty")
	}
	var name string
	if len(parts) == 1 {
		name = parts[0]
	} else {
		name = "/" + strings.Join(parts, "/")
	}
	input := &ssm.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(true),
	}
	output, err := client.GetParameter(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to get parameter: %w", err)
	}
	return mgr.newCredentialsOptionFromBytes(ctx, []byte(*output.Parameter.Value))
}

type credentials struct {
	Type                           string            `json:"type"`
	Audience                       string            `json:"audience"`
	SubjectTokenType               string            `json:"subject_token_type"`
	ServiceAccountImpersonationURL string            `json:"service_account_impersonation_url"`
	TokenURL                       string            `json:"token_url"`
	CredentialSource               *credentialSource `json:"credential_source,omitempty"`
}

type credentialSource struct {
	File                        string `json:"file,omitempty"`
	URL                         string `json:"url,omitempty"`
	EnvironmentID               string `json:"environment_id,omitempty"`
	RegionURL                   string `json:"region_url,omitempty"`
	RegionalCredVerificationURL string `json:"regional_cred_verification_url,omitempty"`
}

func (cred *credentials) notTemporary() bool {
	return cred.Type != "external_account"
}

func (cred *credentials) credentialsType() option.CredentialsType {
	switch cred.Type {
	case "service_account":
		return option.ServiceAccount
	case "authorized_user":
		return option.AuthorizedUser
	case "impersonated_service_account":
		return option.ImpersonatedServiceAccount
	case "external_account":
		return option.ExternalAccount
	default:
		return option.CredentialsType(cred.Type)
	}
}

func (mgr *CredentialsManager) newCredentialsOptionFromBytes(_ context.Context, bs []byte) (option.ClientOption, error) {
	if len(bs) == 0 {
		return nil, errors.New("empty credentials")
	}
	if decoded, err := base64.StdEncoding.DecodeString(string(bs)); err == nil {
		bs = decoded
	}
	if !json.Valid(bs) {
		return nil, errors.New("invalid credentials: not JSON")
	}
	var creds credentials
	if err := json.Unmarshal(bs, &creds); err != nil {
		return nil, fmt.Errorf("failed to unmarshal credentials: %w", err)
	}
	if creds.notTemporary() {
		mgr.setCredentialsCache(bs, &creds)
		return option.WithAuthCredentialsJSON(creds.credentialsType(), bs), nil
	}
	rewrited, err := mgr.rewriteCredentialSource(&creds)
	if err != nil {
		return nil, fmt.Errorf("failed to rewrite credential source: %w", err)
	}
	bs, err = json.Marshal(rewrited)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal credentials: %w", err)
	}
	mgr.setCredentialsCache(bs, rewrited)
	return option.WithAuthCredentialsJSON(rewrited.credentialsType(), bs), nil
}

func (mgr *CredentialsManager) rewriteCredentialSource(cred *credentials) (*credentials, error) {
	// check from AWS Credential Source
	if cred.SubjectTokenType != SubjectTokenTypeForAWS {
		return cred, nil
	}
	if cred.CredentialSource != nil && cred.CredentialSource.File != "" {
		return cred, nil
	}

	addr, err := mgr.getProxyServerAddress()
	if err != nil {
		return nil, fmt.Errorf("failed to get proxy server address: %w", err)
	}
	if cred.CredentialSource == nil {
		cred.CredentialSource = &credentialSource{
			EnvironmentID: "aws1",
		}
	}
	cred.CredentialSource.URL = fmt.Sprintf("http://%s%s", addr, credentialsPath)
	cred.CredentialSource.RegionURL = fmt.Sprintf("http://%s%s", addr, regionPath)
	cred.CredentialSource.RegionalCredVerificationURL = "https://sts.{region}.amazonaws.com?Action=GetCallerIdentity&Version=2011-06-15"
	return cred, nil
}

const (
	regionPath      = "/latest/meta-data/placement/availability-zone"
	credentialsPath = "/latest/meta-data/iam/security-credentials"
)

func (mgr *CredentialsManager) getProxyServerAddress() (string, error) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.proxyServer == nil {
		listener, err := net.Listen("tcp", ":0")
		if err != nil {
			return "", fmt.Errorf("failed to listen: %w", err)
		}
		mgr.proxyListener = listener
		if region := os.Getenv("AWS_REGION"); region != "" {
			mgr.proxyRegion = region
		} else if region := os.Getenv("AWS_DEFAULT_REGION"); region != "" {
			mgr.proxyRegion = region
		} else {
			mgr.proxyRegion = "us-east-1"
		}
		m := http.NewServeMux()
		m.HandleFunc(regionPath, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(mgr.proxyRegion))
		})
		m.HandleFunc(credentialsPath, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("default"))
		})
		m.HandleFunc(credentialsPath+"/default", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			mgr.mu.Lock()
			defer mgr.mu.Unlock()
			awsCfg, err := mgr.loadConfig(r.Context())
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, `{"Code": "Failed", "Message": "%s"}`, err.Error())
				return
			}
			cloned := awsCfg.Copy()
			cloned.Region = mgr.proxyRegion
			cred, err := cloned.Credentials.Retrieve(r.Context())
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, `{"Code": "Failed", "Message": "%s"}`, err.Error())
				return
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w,
				`{"Code": "Success", "LastUpdated":"%s", "Type": "AWS-HMAC", "AccessKeyId": "%s", "SecretAccessKey": "%s", "Token": "%s", "Expiration": "%s"}`,
				time.Now().Format(time.RFC3339),
				cred.AccessKeyID,
				cred.SecretAccessKey,
				cred.SessionToken,
				cred.Expires.Format(time.RFC3339),
			)
		})
		mgr.proxyServer = &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				logger := mgr.getLogger()
				logger.Debug("receive request on credentials proxy server", "method", r.Method, "url", r.URL, "remote_addr", r.RemoteAddr)
				m.ServeHTTP(w, r)
			}),
		}
		mgr.proxyWaitGroup = sync.WaitGroup{}
		mgr.proxyWaitGroup.Add(1)
		go func() {
			logger := mgr.getLogger()
			logger.Info("start credentials proxy server", "addr", listener.Addr())
			if err := mgr.proxyServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("failed to serve credentials proxy server", "error", err)
			}
			mgr.proxyWaitGroup.Done()
		}()
	}
	port := mgr.proxyListener.Addr().(*net.TCPAddr).Port
	return fmt.Sprintf("127.0.0.1:%d", port), nil
}

func (mgr *CredentialsManager) setCredentialsCache(bs []byte, cred *credentials) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.cacheCredentialsJSON = bs
	mgr.cacheCredentials = cred
	mgr.cacheCredentialsExpiresAt = time.Now().Add(CacheLifetimeSeconds * time.Second)
}

func (mgr *CredentialsManager) getCachedCredentials() ([]byte, *credentials, bool) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.cacheCredentials == nil {
		return nil, nil, false
	}
	if mgr.cacheCredentialsExpiresAt.Before(time.Now()) {
		return nil, nil, false
	}
	return mgr.cacheCredentialsJSON, mgr.cacheCredentials, true
}
