// Package ovo is the provider adapter for OVO SNAP Dynamic QRIS.
//
// Confirmation model: inquiry first. A payment is confirmed only by OVO's
// Transaction Inquiry (qr-mpm-query), through the payment engine's normal
// validation. OVO callbacks (qr-mpm-notify) are not supported yet because
// their verification scheme is not publicly documented; see
// docs/providers.md for this and the other open onboarding items.
//
// Implemented from OVO's Technical Documentation v1.4 and the official
// Postman collection "[SNAP] Dynamic QRIS V2". Nothing here has been verified
// against OVO's sandbox.
package ovo

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"merchant-payments/internal/domain"
	"merchant-payments/internal/id"
	"merchant-payments/internal/provider"
)

// Name is the provider code: API requests use "provider":"ovo".
const Name domain.Provider = "ovo"

const (
	pathGenerateQR = "/OVOSNAP/v2.0/qr/qr-mpm-generate"
	pathQueryQR    = "/OVOSNAP/v2.0/qr/qr-mpm-query"

	// serviceCodeGenerateQR identifies the original QR generation in an
	// inquiry ("Service Code 47").
	serviceCodeGenerateQR = "47"
	// channelOVO is additionalInfo.channel, documented as "String = 'OVO'".
	// OVO's examples also show "ovo"; confirm during onboarding.
	channelOVO = "OVO"

	// Documented QR expiry bounds and default, in seconds.
	MinQRExpirySeconds     = 75
	MaxQRExpirySeconds     = 86400
	defaultQRExpirySeconds = 75
)

// Config holds the OVO merchant settings. ClientSecret and PrivateKeyPEM are
// secrets.
type Config struct {
	BaseURL        string // e.g. OVO's staging or production host; never hard-coded
	ClientID       string // X-CLIENT-KEY
	ClientSecret   string // HMAC-SHA512 key
	PrivateKeyPEM  []byte // RSA key for the B2B token signature (PKCS#1 or PKCS#8 PEM)
	PartnerID      string // X-PARTNER-ID
	ChannelID      string // CHANNEL-ID
	MerchantID     string // merchantId
	TerminalID     string // terminalId
	DeviceID       string // additionalInfo.deviceId ("Can be Hardcoded/Static String")
	AllowedSources string // additionalInfo.allowedSources bitmap, per the merchant's OVO agreement
	// QRExpirySeconds is sent as "expiry" when non-zero (75–86400). Zero
	// omits it, and OVO applies its documented default of 75 seconds.
	QRExpirySeconds int
	// Location is the client's local time zone. OVO requires X-TIMESTAMP to
	// be the "Client's current local time" with its offset, and
	// transactionDate (YYMMDD) uses the same zone. Required; the server
	// passes the runtime zone (TZ).
	Location *time.Location
	// HTTPClient is optional; the engine bounds every call with a context
	// deadline.
	HTTPClient *http.Client
}

// Adapter implements provider.Adapter for OVO SNAP Dynamic QRIS.
type Adapter struct {
	cfg           Config
	key           *rsa.PrivateKey
	http          *http.Client
	now           func() time.Time
	newExternalID func() string

	tokenMu     sync.Mutex
	token       string
	tokenExpiry time.Time
}

var (
	_ provider.Adapter = (*Adapter)(nil)

	allowedSourcesPattern = regexp.MustCompile(`^[01]+$`)
)

// New validates cfg and returns an adapter. Errors name the offending
// setting, never its value.
func New(cfg Config) (*Adapter, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" && u.Path != "/" {
		return nil, errors.New("ovo: base URL must be an absolute URL without a path")
	}
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
	for name, v := range map[string]string{
		"client ID": cfg.ClientID, "client secret": cfg.ClientSecret, "partner ID": cfg.PartnerID,
		"channel ID": cfg.ChannelID, "merchant ID": cfg.MerchantID, "terminal ID": cfg.TerminalID,
		"device ID": cfg.DeviceID,
	} {
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("ovo: %s is required", name)
		}
	}
	if !allowedSourcesPattern.MatchString(cfg.AllowedSources) || !strings.Contains(cfg.AllowedSources, "1") {
		return nil, errors.New("ovo: allowed sources must be a bitmap of 0/1 with at least one source enabled")
	}
	if cfg.Location == nil {
		return nil, errors.New("ovo: local time zone is required")
	}
	if cfg.QRExpirySeconds != 0 && (cfg.QRExpirySeconds < MinQRExpirySeconds || cfg.QRExpirySeconds > MaxQRExpirySeconds) {
		return nil, fmt.Errorf("ovo: QR expiry must be between %d and %d seconds", MinQRExpirySeconds, MaxQRExpirySeconds)
	}
	key, err := parsePrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	return &Adapter{
		cfg:           cfg,
		key:           key,
		http:          client,
		now:           time.Now,
		newExternalID: id.New, // 36 characters, the documented maximum
	}, nil
}

func parsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("ovo: private key is not PEM encoded")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("ovo: private key is not a valid PKCS#1 RSA key")
		}
		return key, nil
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		key, ok := parsed.(*rsa.PrivateKey)
		if err != nil || !ok {
			return nil, errors.New("ovo: private key is not a valid PKCS#8 RSA key")
		}
		return key, nil
	}
	return nil, errors.New("ovo: unsupported private key PEM type")
}

// localTime converts t to the configured local time zone.
func (a *Adapter) localTime(t time.Time) time.Time { return t.In(a.cfg.Location) }

func (a *Adapter) Name() domain.Provider { return Name }

// SupportsMethod accepts QRIS only. OVO PayLater is a funding source of a
// QRIS payment (allowedSources), reported by inquiry; it is not offered as a
// separate payment method.
func (a *Adapter) SupportsMethod(m domain.PaymentMethod) bool { return m == domain.PaymentMethodQRIS }

type amount struct {
	Value    string `json:"value"`
	Currency string `json:"currency,omitempty"`
}

type generateRequest struct {
	PartnerReferenceNo string                 `json:"partnerReferenceNo"`
	TerminalID         string                 `json:"terminalId"`
	MerchantID         string                 `json:"merchantId"`
	Amount             amount                 `json:"amount"`
	AdditionalInfo     generateAdditionalInfo `json:"additionalInfo"`
	Expiry             int                    `json:"expiry,omitempty"`
}

type generateAdditionalInfo struct {
	DeviceID        string `json:"deviceId"`
	Channel         string `json:"channel"`
	TransactionDate string `json:"transactionDate"`
	AllowedSources  string `json:"allowedSources"`
}

type generateResponse struct {
	responseHeader
	ReferenceNo        string `json:"referenceNo"`
	PartnerReferenceNo string `json:"partnerReferenceNo"`
	QRContent          string `json:"qrContent"`
	AdditionalInfo     struct {
		Expiry json.RawMessage `json:"expiry"`
	} `json:"additionalInfo"`
}

// CreatePayment generates a Dynamic QR. It never confirms payment: the
// result only moves the payment to PENDING with OVO's referenceNo, the
// qrContent exactly as returned, and the QR expiry.
//
// Errors: provider.ErrRejected when OVO definitively refuses (HTTP 400, 401
// after one token refresh, 404, 429) or the request cannot be sent in a
// supported form; any other failure (timeouts, 5xx, 409 duplicates, network
// errors, unexpected bodies) is an unknown outcome.
func (a *Adapter) CreatePayment(ctx context.Context, req provider.CreateRequest) (provider.CreateResult, error) {
	if req.Method != domain.PaymentMethodQRIS {
		return provider.CreateResult{}, fmt.Errorf("%w: ovo: payment method %s not supported", provider.ErrRejected, req.Method)
	}
	if req.Currency != currencyIDR {
		return provider.CreateResult{}, fmt.Errorf("%w: ovo: currency %s not supported", provider.ErrRejected, req.Currency)
	}
	value, err := formatAmount(req.Amount)
	if err != nil {
		return provider.CreateResult{}, fmt.Errorf("%w: %v", provider.ErrRejected, err)
	}

	// sentAt is read before the request, so expires_at is never later than
	// OVO's actual expiry.
	sentAt := a.now()
	res, err := a.transact(ctx, "generate QR", pathGenerateQR, func(now time.Time) any {
		return generateRequest{
			PartnerReferenceNo: req.MerchantReference,
			TerminalID:         a.cfg.TerminalID,
			MerchantID:         a.cfg.MerchantID,
			Amount:             amount{Value: value, Currency: currencyIDR},
			AdditionalInfo: generateAdditionalInfo{
				DeviceID: a.cfg.DeviceID,
				Channel:  channelOVO,
				// YYMMDD from the same instant as this attempt's X-TIMESTAMP.
				TransactionDate: now.Format("060102"),
				AllowedSources:  a.cfg.AllowedSources,
			},
			Expiry: a.cfg.QRExpirySeconds,
		}
	})
	if err != nil {
		return provider.CreateResult{}, err
	}

	var resp generateResponse
	if err := json.Unmarshal(res.body, &resp); err != nil {
		return provider.CreateResult{}, newProviderError("generate QR", res)
	}
	if res.status != http.StatusOK || !isSuccessCode(resp.ResponseCode) {
		perr := newProviderError("generate QR", res)
		switch res.status {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusTooManyRequests:
			return provider.CreateResult{}, fmt.Errorf("%w: %v", provider.ErrRejected, perr)
		}
		return provider.CreateResult{}, perr
	}
	switch {
	case resp.ReferenceNo == "":
		return provider.CreateResult{}, errors.New("ovo: generate QR: success response without referenceNo")
	case resp.QRContent == "":
		// qrUrl/qrImage alternatives are documented but not supported.
		return provider.CreateResult{}, errors.New("ovo: generate QR: success response without qrContent")
	case resp.PartnerReferenceNo != "" && resp.PartnerReferenceNo != req.MerchantReference:
		return provider.CreateResult{}, errors.New("ovo: generate QR: response partnerReferenceNo does not match the request")
	}

	expiresAt := sentAt.Add(time.Duration(a.qrExpirySeconds(resp.AdditionalInfo.Expiry)) * time.Second)
	return provider.CreateResult{
		ProviderReference: resp.ReferenceNo,
		Action:            domain.CustomerAction{Type: domain.ActionQRString, Value: resp.QRContent},
		ExpiresAt:         &expiresAt,
	}, nil
}

// qrExpirySeconds returns OVO's additionalInfo.expiry when it is a number
// within the documented bounds; otherwise the requested expiry, or OVO's
// documented default when none was requested.
func (a *Adapter) qrExpirySeconds(raw json.RawMessage) int {
	var secs int
	if json.Unmarshal(raw, &secs) == nil && secs >= MinQRExpirySeconds && secs <= MaxQRExpirySeconds {
		return secs
	}
	if a.cfg.QRExpirySeconds != 0 {
		return a.cfg.QRExpirySeconds
	}
	return defaultQRExpirySeconds
}

type queryRequest struct {
	OriginalReferenceNo        string              `json:"originalReferenceNo"`
	OriginalPartnerReferenceNo string              `json:"originalPartnerReferenceNo"`
	ServiceCode                string              `json:"serviceCode"`
	MerchantID                 string              `json:"merchantId"`
	AdditionalInfo             queryAdditionalInfo `json:"additionalInfo"`
}

type queryAdditionalInfo struct {
	DeviceID   string `json:"deviceId"`
	Channel    string `json:"channel"`
	TerminalID string `json:"terminalId"`
}

type queryResponse struct {
	responseHeader
	OriginalReferenceNo        string              `json:"originalReferenceNo"`
	OriginalPartnerReferenceNo string              `json:"originalPartnerReferenceNo"`
	LatestTransactionStatus    string              `json:"latestTransactionStatus"`
	TransactionStatusDesc      string              `json:"transactionStatusDesc"`
	PaidTime                   string              `json:"paidTime"`
	Amount                     *amount             `json:"amount"`
	TransactionSummary         *transactionSummary `json:"transactionSummary"`
	AdditionalInfo             *struct {
		TransactionID      json.RawMessage     `json:"transactionId"`
		BatchNo            json.RawMessage     `json:"batchNo"`
		TransactionSummary *transactionSummary `json:"transactionSummary"`
	} `json:"additionalInfo"`
}

// GetPaymentStatus calls Transaction Inquiry and translates the answer. The
// engine validates provider, references, amount, currency and status before
// applying it. Any failure, including "Transaction Not Found", leaves the
// payment unchanged.
func (a *Adapter) GetPaymentStatus(ctx context.Context, req provider.StatusRequest) (domain.StatusReport, error) {
	if req.ProviderReference == "" {
		// originalReferenceNo is mandatory; it is unknown when QR generation
		// timed out before OVO's referenceNo was received.
		return domain.StatusReport{}, errors.New("ovo: inquiry needs OVO's referenceNo, which is unknown for this payment")
	}
	inquiry := queryRequest{
		OriginalReferenceNo:        req.ProviderReference,
		OriginalPartnerReferenceNo: req.MerchantReference,
		ServiceCode:                serviceCodeGenerateQR,
		MerchantID:                 a.cfg.MerchantID,
		AdditionalInfo: queryAdditionalInfo{
			DeviceID: a.cfg.DeviceID, Channel: channelOVO, TerminalID: a.cfg.TerminalID,
		},
	}
	res, err := a.transact(ctx, "inquiry", pathQueryQR, func(time.Time) any { return inquiry })
	if err != nil {
		return domain.StatusReport{}, err
	}
	var resp queryResponse
	if err := json.Unmarshal(res.body, &resp); err != nil || res.status != http.StatusOK || !isSuccessCode(resp.ResponseCode) {
		return domain.StatusReport{}, newProviderError("inquiry", res)
	}
	return reportFromInquiry(resp)
}

// reportFromInquiry builds a status report from a successful inquiry
// response. References are taken only from OVO's answer, so the engine's
// comparison with the payment is real evidence.
func reportFromInquiry(resp queryResponse) (domain.StatusReport, error) {
	status, err := mapStatus(resp.LatestTransactionStatus)
	if err != nil {
		return domain.StatusReport{}, err
	}
	if resp.OriginalReferenceNo == "" || resp.OriginalPartnerReferenceNo == "" {
		return domain.StatusReport{}, errors.New("ovo: inquiry response without transaction references")
	}

	report := domain.StatusReport{
		Provider:          Name,
		ProviderReference: resp.OriginalReferenceNo,
		MerchantReference: resp.OriginalPartnerReferenceNo,
		Status:            status,
		FundingSource:     domain.FundingSourceUnknown,
		OccurredAt:        parsePaidTime(resp.PaidTime),
	}
	if resp.Amount != nil && resp.Amount.Value != "" {
		v, err := parseAmount(resp.Amount.Value)
		if err != nil {
			return domain.StatusReport{}, err
		}
		report.Amount = &v
		// Left empty when OVO omits it; the engine then refuses to confirm.
		report.Currency = resp.Amount.Currency
	}

	payload := auditPayload{
		ResponseCode:               resp.ResponseCode,
		OriginalReferenceNo:        resp.OriginalReferenceNo,
		OriginalPartnerReferenceNo: resp.OriginalPartnerReferenceNo,
		LatestTransactionStatus:    resp.LatestTransactionStatus,
		TransactionStatusDesc:      resp.TransactionStatusDesc,
		PaidTime:                   resp.PaidTime,
		Amount:                     resp.Amount,
		TransactionSummary:         resp.TransactionSummary,
	}
	var nested *transactionSummary
	if info := resp.AdditionalInfo; info != nil {
		nested = info.TransactionSummary
		payload.AdditionalInfoTransactionSummary = info.TransactionSummary
		payload.TransactionID, payload.BatchNo = info.TransactionID, info.BatchNo
	}
	if status == domain.PaymentStatusPaid {
		report.FundingSource = fundingSource(effectiveSummary(resp.TransactionSummary, nested), report.Amount)
	}
	if report.Payload, err = json.Marshal(payload); err != nil {
		return domain.StatusReport{}, err
	}
	return report, nil
}

// ParseWebhook rejects every request: OVO callbacks are not supported yet
// because OVO does not publicly document how to verify them. Payments are
// confirmed by inquiry, and nothing is stored for rejected callbacks.
func (a *Adapter) ParseWebhook(context.Context, provider.Webhook) (domain.StatusReport, error) {
	return domain.StatusReport{}, fmt.Errorf("%w: OVO callbacks are not supported; payments are confirmed by inquiry",
		provider.ErrInvalidSignature)
}
