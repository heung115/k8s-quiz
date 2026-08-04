package v1

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultMaxResponseBody = 64 << 10

type ClientConfig struct {
	BaseURL            string
	TLSConfig          *tls.Config
	ExpectedServerURI  string
	ProviderID         string
	AuthorityIssuerURI string
	ProofIssuer        AuthorityProofIssuer
	RequestTimeout     time.Duration
	MaxResponseBody    int64
	Transport          *http.Transport
}

type Client struct {
	baseURL              *url.URL
	httpClient           *http.Client
	maxResponseBody      int64
	providerID           string
	authorityIssuerURI   string
	authorityAudienceURI string
	proofIssuer          AuthorityProofIssuer
}

func NewClient(config ClientConfig) (*Client, error) {
	baseURL, err := url.Parse(config.BaseURL)
	if err != nil || baseURL.Scheme != "https" || baseURL.Host == "" || baseURL.User != nil ||
		baseURL.RawQuery != "" || baseURL.Fragment != "" || (baseURL.Path != "" && baseURL.Path != "/") {
		return nil, errors.New("private Runner client base URL is invalid")
	}
	if config.TLSConfig == nil || config.TLSConfig.MinVersion < tls.VersionTLS13 ||
		config.TLSConfig.InsecureSkipVerify || len(config.TLSConfig.Certificates) == 0 || config.TLSConfig.RootCAs == nil {
		return nil, errors.New("private Runner client requires verified TLS 1.3 mutual authentication")
	}
	expectedServerURI, err := url.Parse(config.ExpectedServerURI)
	if err != nil || expectedServerURI.Scheme == "" || expectedServerURI.Host == "" ||
		!ValidProviderID(config.ProviderID) || !CanonicalAuthorityURI(config.AuthorityIssuerURI) ||
		!CanonicalAuthorityURI(config.ExpectedServerURI) || config.ProofIssuer == nil {
		return nil, errors.New("private Runner client expected server URI is invalid")
	}
	hardenedTLS := config.TLSConfig.Clone()
	priorVerifyConnection := hardenedTLS.VerifyConnection
	hardenedTLS.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 ||
			!hasExplicitUsage(state.PeerCertificates[0], x509.ExtKeyUsageServerAuth) ||
			!hasExactURI(state.PeerCertificates[0], expectedServerURI) {
			return ErrUnauthorized
		}
		if priorVerifyConnection != nil {
			return priorVerifyConnection(state)
		}
		return nil
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 30 * time.Second
	}
	if config.MaxResponseBody == 0 {
		config.MaxResponseBody = defaultMaxResponseBody
	}
	if config.RequestTimeout <= 0 || config.MaxResponseBody < 1024 {
		return nil, errors.New("private Runner client limits are invalid")
	}
	transport := config.Transport
	if transport == nil {
		transport = &http.Transport{
			TLSClientConfig: hardenedTLS, TLSHandshakeTimeout: 5 * time.Second,
			ResponseHeaderTimeout: config.RequestTimeout, IdleConnTimeout: 60 * time.Second,
			MaxIdleConns: 4, MaxIdleConnsPerHost: 2, MaxConnsPerHost: 4, ForceAttemptHTTP2: true,
		}
	} else {
		transport = transport.Clone()
		transport.TLSClientConfig = hardenedTLS
		if transport.TLSHandshakeTimeout == 0 {
			transport.TLSHandshakeTimeout = 5 * time.Second
		}
		if transport.ResponseHeaderTimeout == 0 {
			transport.ResponseHeaderTimeout = config.RequestTimeout
		}
	}
	baseURL.Path = ""
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Transport: transport, Timeout: config.RequestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("private Runner redirects are forbidden")
			},
		},
		maxResponseBody: config.MaxResponseBody, providerID: config.ProviderID,
		authorityIssuerURI:   config.AuthorityIssuerURI,
		authorityAudienceURI: config.ExpectedServerURI, proofIssuer: config.ProofIssuer,
	}, nil
}

func (c *Client) CloseIdleConnections() {
	if c != nil && c.httpClient != nil {
		c.httpClient.CloseIdleConnections()
	}
}

func (c *Client) ActivateController(ctx context.Context, request ActivateRequest) error {
	proof, err := c.issueProof(ctx, OperationActivate, request.ControllerEpoch, request.EffectDeadline, request)
	if err != nil {
		return err
	}
	request.AuthorityProof = proof
	var response ActivateResponse
	if err := c.call(ctx, PathActivate, request, &response, true); err != nil {
		return err
	}
	if response.Schema != Schema || response.AcceptedControllerEpoch != request.ControllerEpoch {
		return mutationResponseViolation()
	}
	return nil
}

func (c *Client) Create(ctx context.Context, request CreateRequest) (AllocationView, error) {
	proof, err := c.issueProof(ctx, OperationCreate, request.ControllerEpoch, request.EffectDeadline, request)
	if err != nil {
		return AllocationView{}, err
	}
	request.AuthorityProof = proof
	var response AllocationResponse
	if err := c.call(ctx, PathCreate, request, &response, true); err != nil {
		return AllocationView{}, err
	}
	if err := validateAllocationResponse(response, request.ControllerEpoch, AllocationIdentity{
		AllocationID: request.AllocationID, SessionID: request.SessionID, Generation: request.Generation,
	}); err != nil {
		return AllocationView{}, mutationResponseViolation()
	}
	if response.Allocation.CatalogGeneration != request.CatalogGeneration {
		return AllocationView{}, mutationResponseViolation()
	}
	return response.Allocation, nil
}

func (c *Client) Get(ctx context.Context, request GetRequest) (AllocationView, error) {
	proof, err := c.issueProof(ctx, OperationGet, request.ControllerEpoch, request.EffectDeadline, request)
	if err != nil {
		return AllocationView{}, err
	}
	request.AuthorityProof = proof
	var response AllocationResponse
	if err := c.call(ctx, PathGet, request, &response, false); err != nil {
		return AllocationView{}, err
	}
	if err := validateAllocationResponse(response, request.ControllerEpoch, AllocationIdentity{
		AllocationID: request.AllocationID, SessionID: request.SessionID, Generation: request.Generation,
	}); err != nil {
		return AllocationView{}, err
	}
	return response.Allocation, nil
}

func (c *Client) Destroy(ctx context.Context, request DestroyRequest) (AllocationView, error) {
	proof, err := c.issueProof(ctx, OperationDestroy, request.ControllerEpoch, request.EffectDeadline, request)
	if err != nil {
		return AllocationView{}, err
	}
	request.AuthorityProof = proof
	var response AllocationResponse
	if err := c.call(ctx, PathDestroy, request, &response, true); err != nil {
		return AllocationView{}, err
	}
	if err := validateAllocationResponse(response, request.ControllerEpoch, AllocationIdentity{
		AllocationID: request.AllocationID, SessionID: request.SessionID, Generation: request.Generation,
	}); err != nil {
		return AllocationView{}, mutationResponseViolation()
	}
	return response.Allocation, nil
}

func mutationResponseViolation() error {
	// A syntactically valid success response is still not evidence that a
	// mutation had no effect when its semantic binding is invalid. Preserve the
	// ambiguity so callers reconcile or retry the exact idempotent operation.
	return errors.Join(ErrOutcomeUnknown, ErrProtocolViolation)
}

func (c *Client) issueProof(ctx context.Context, operation string, epoch uint64, encodedDeadline string, request any) (AuthorityProof, error) {
	if c == nil || c.proofIssuer == nil {
		return AuthorityProof{}, ErrUnauthorized
	}
	deadline, err := ParseCanonicalDatabaseUTC(encodedDeadline)
	if err != nil {
		return AuthorityProof{}, ErrInvalidRequest
	}
	digest, err := RequestDigest(c.providerID, request)
	if err != nil {
		return AuthorityProof{}, ErrInvalidRequest
	}
	proof, err := c.proofIssuer.IssueAuthorityProof(ctx, AuthorityProofRequest{
		Operation: operation, RequestDigest: digest, EffectDeadline: deadline,
		IssuerURI: c.authorityIssuerURI, AudienceURI: c.authorityAudienceURI,
	})
	if err != nil {
		return AuthorityProof{}, err
	}
	if _, err := ValidateAuthorityProof(proof, AuthorityProofBinding{
		ProviderID: c.providerID, Epoch: epoch, Operation: operation, RequestDigest: digest,
		EffectDeadline: encodedDeadline, IssuerURI: c.authorityIssuerURI,
		AudienceURI: c.authorityAudienceURI,
	}); err != nil {
		return AuthorityProof{}, ErrUnauthorized
	}
	return proof, nil
}

func validateAllocationResponse(response AllocationResponse, epoch uint64, identity AllocationIdentity) error {
	allocation := response.Allocation
	if response.Schema != Schema || response.AcceptedControllerEpoch != epoch ||
		allocation.AllocationID != identity.AllocationID || allocation.SessionID != identity.SessionID ||
		allocation.Generation != identity.Generation || allocation.CatalogGeneration == 0 ||
		allocation.LifecycleScope != LifecycleScopeVMOnly || allocation.VMPhase == "" ||
		allocation.VMObservedState == "" || allocation.ExpiresAt == "" {
		return ErrProtocolViolation
	}
	if !allowedDesiredState(allocation.DesiredState) || !allowedVMPhase(allocation.VMPhase) ||
		!allowedVMObservedState(allocation.VMObservedState) || !allowedAllocationErrorCode(allocation.ErrorCode) {
		return ErrProtocolViolation
	}
	return nil
}

func allowedDesiredState(value string) bool { return value == "active" || value == "absent" }

func allowedVMPhase(value string) bool {
	switch value {
	case "vm_provisioning", "vm_configuring", "vm_starting", "vm_running",
		"vm_destroying", "vm_absent", "vm_cleanup_required", "vm_provider_lost":
		return true
	default:
		return false
	}
}

func allowedVMObservedState(value string) bool {
	switch value {
	case "vm_unknown", "vm_stopped", "vm_running", "vm_deleting", "vm_absent", "vm_error":
		return true
	default:
		return false
	}
}

func allowedAllocationErrorCode(value string) bool {
	return value == "" || value == string(CodeCleanupRequired) || value == string(CodeProviderUnavailable)
}

func (c *Client) call(ctx context.Context, path string, input, output any, outcomeMayBeUnknown bool) error {
	if c == nil || c.httpClient == nil || c.baseURL == nil {
		return ErrProtocolViolation
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return ErrInvalidRequest
	}
	endpoint := *c.baseURL
	endpoint.Path = path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return ErrInvalidRequest
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		if outcomeMayBeUnknown {
			return errors.Join(ErrOutcomeUnknown, err)
		}
		return errors.Join(ErrProviderUnavailable, err)
	}
	defer response.Body.Close()
	mediaType, parameters, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" || len(parameters) != 0 || response.Header.Get("Content-Encoding") != "" {
		if outcomeMayBeUnknown {
			// The mutation already reached an authenticated peer. An untrusted or
			// malformed response cannot prove absence of its external effect,
			// regardless of the asserted HTTP status.
			return errors.Join(ErrOutcomeUnknown, ErrProtocolViolation)
		}
		return ErrProtocolViolation
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBody+1))
	if readErr != nil || int64(len(body)) > c.maxResponseBody {
		if outcomeMayBeUnknown {
			return errors.Join(ErrOutcomeUnknown, ErrProtocolViolation)
		}
		return ErrProtocolViolation
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var errorResponse ErrorResponse
		if err := DecodeStrict(body, &errorResponse); err != nil || errorResponse.Schema != ErrorSchema ||
			len(errorResponse.Error.Message) == 0 || len(errorResponse.Error.Message) > 512 {
			if outcomeMayBeUnknown {
				return errors.Join(ErrOutcomeUnknown, ErrProtocolViolation)
			}
			return ErrProtocolViolation
		}
		expected, ok := ErrorContract(errorResponse.Error.Code)
		if !ok || response.StatusCode != expected.Status || errorResponse.Error.Retryable != expected.Retryable ||
			errorResponse.Error.Message != expected.Message {
			if outcomeMayBeUnknown {
				return errors.Join(ErrOutcomeUnknown, ErrProtocolViolation)
			}
			return ErrProtocolViolation
		}
		return &RemoteError{Code: errorResponse.Error.Code, Retryable: errorResponse.Error.Retryable, Message: errorResponse.Error.Message}
	}
	if err := DecodeStrict(body, output); err != nil {
		if outcomeMayBeUnknown {
			return errors.Join(ErrOutcomeUnknown, ErrProtocolViolation)
		}
		return ErrProtocolViolation
	}
	return nil
}

func (c *Client) URL() string {
	if c == nil || c.baseURL == nil {
		return ""
	}
	return strings.TrimSuffix(c.baseURL.String(), "/")
}
