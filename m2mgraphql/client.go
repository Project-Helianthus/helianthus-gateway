package m2mgraphql

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
	"os"
	"strings"
	"time"
)

const clientTimeout = 5 * time.Second

type ClientConfig struct {
	URL            string
	ServerName     string
	CAFile         string
	ClientCertFile string
	ClientKeyFile  string
	AssetRef       string
}

type Client struct {
	url   string
	asset string
	http  *http.Client
}

type Response struct {
	Status      int
	ContentType string
	Body        []byte
}

func NewClient(config ClientConfig) (*Client, error) {
	for _, value := range []string{config.URL, config.ServerName, config.CAFile, config.ClientCertFile, config.ClientKeyFile, config.AssetRef} {
		if strings.TrimSpace(value) == "" {
			return nil, errors.New("M2M GraphQL client configuration is incomplete")
		}
	}
	parsedURL, err := url.Parse(config.URL)
	if err != nil || !parsedURL.IsAbs() || parsedURL.Scheme != "https" || parsedURL.Host == "" ||
		parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" || parsedURL.Opaque != "" ||
		parsedURL.ForceQuery || parsedURL.Path != route || parsedURL.RawPath != "" {
		return nil, errors.New("M2M GraphQL client URL is invalid")
	}
	certificate, err := tls.LoadX509KeyPair(config.ClientCertFile, config.ClientKeyFile)
	if err != nil {
		return nil, errors.New("M2M GraphQL client certificate is invalid")
	}
	caPEM, err := os.ReadFile(config.CAFile)
	if err != nil {
		return nil, errors.New("M2M GraphQL client CA is unavailable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("M2M GraphQL client CA is invalid")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: config.ServerName, Certificates: []tls.Certificate{certificate}}, ResponseHeaderTimeout: clientTimeout}
	return &Client{url: config.URL, asset: config.AssetRef, http: &http.Client{Transport: transport, Timeout: clientTimeout}}, nil
}

func (client *Client) Current(ctx context.Context) (Response, error) {
	return client.current(ctx, "SemanticPVCurrent", semanticPVFixedQuery, semanticPVContractID)
}

func (client *Client) StorageCurrent(ctx context.Context) (Response, error) {
	return client.current(ctx, "SemanticStorageCurrent", semanticStorageFixedQuery, semanticStorageContractID)
}

// EVSECurrent uses the fixed authenticated EVSE SemReg query.  It has no
// native-record fallback and no operation path.
func (client *Client) EVSECurrent(ctx context.Context) (Response, error) {
	return client.current(ctx, "SemanticEVSECurrent", semanticEVSEFixedQuery, semanticEVSEContractID)
}

func (client *Client) current(ctx context.Context, operation, query, contract string) (Response, error) {
	return client.post(ctx, operation, query, map[string]any{"request": map[string]string{"contract_id": contract, "asset_ref": client.asset}})
}

// GraphEntry names the resource or lens a traversal or lens read starts from.
type GraphEntry struct {
	Kind     string `json:"kind"`
	Resource string `json:"resource,omitempty"`
	Lens     string `json:"lens,omitempty"`
}

type GraphPackRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type GraphDefinitionRef struct {
	ID      string       `json:"id"`
	Version string       `json:"version"`
	Pack    GraphPackRef `json:"pack"`
}

// GraphPage carries the cut pin and paging arguments shared by the reads.
type GraphPage struct {
	CutID     string `json:"cut_id,omitempty"`
	PageSize  int    `json:"page_size"`
	PageToken string `json:"page_token,omitempty"`
}

type SemanticResourcesRequest struct {
	GraphPage
	Kind  string `json:"kind,omitempty"`
	Plane string `json:"plane,omitempty"`
}

type SemanticTraversalRequest struct {
	GraphPage
	Entry       GraphEntry           `json:"entry"`
	Direction   string               `json:"direction"`
	Depth       int                  `json:"depth"`
	Lens        string               `json:"lens,omitempty"`
	Planes      []string             `json:"planes,omitempty"`
	Definitions []GraphDefinitionRef `json:"definitions,omitempty"`
}

type SemanticLensRequest struct {
	GraphPage
	Entry     GraphEntry `json:"entry"`
	Direction string     `json:"direction"`
	Depth     int        `json:"depth"`
	Lens      string     `json:"lens"`
}

func (client *Client) SemanticResources(ctx context.Context, request SemanticResourcesRequest) (Response, error) {
	return client.graph(ctx, "SemanticResources", semanticResourcesFixedQuery, request)
}

func (client *Client) SemanticTraversal(ctx context.Context, request SemanticTraversalRequest) (Response, error) {
	return client.graph(ctx, "SemanticTraversal", semanticTraversalFixedQuery, request)
}

func (client *Client) SemanticLens(ctx context.Context, request SemanticLensRequest) (Response, error) {
	return client.graph(ctx, "SemanticLens", semanticLensFixedQuery, request)
}

func (client *Client) SemanticElementCatalog(ctx context.Context) (Response, error) {
	return client.graph(ctx, "SemanticElementCatalog", semanticElementCatalogFixedQuery, struct{}{})
}

func (client *Client) graph(ctx context.Context, operation, query string, request any) (Response, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return Response{}, errors.New("M2M GraphQL request is invalid")
	}
	var variables map[string]any
	if err := json.Unmarshal(encoded, &variables); err != nil {
		return Response{}, errors.New("M2M GraphQL request is invalid")
	}
	return client.post(ctx, operation, query, variables)
}

func (client *Client) post(ctx context.Context, operation, query string, variables map[string]any) (Response, error) {
	if client == nil || client.http == nil {
		return Response{}, errors.New("M2M GraphQL client unavailable")
	}
	body, err := json.Marshal(map[string]any{"operationName": operation, "query": query, "variables": variables})
	if err != nil {
		return Response{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.url, bytes.NewReader(body))
	if err != nil {
		return Response{}, errors.New("M2M GraphQL request is invalid")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return Response{}, errors.New("M2M GraphQL request failed")
	}
	defer func() { _ = response.Body.Close() }()
	mediaType, _, contentTypeErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || contentTypeErr != nil || mediaType != "application/json" {
		return Response{}, errors.New("M2M GraphQL response is invalid")
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(encoded) > maxResponseBytes || !json.Valid(encoded) {
		return Response{}, errors.New("M2M GraphQL response is invalid")
	}
	return Response{Status: response.StatusCode, ContentType: response.Header.Get("Content-Type"), Body: encoded}, nil
}
