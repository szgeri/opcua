// Copyright 2021 Converter Systems LLC. All rights reserved.

package client

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"os"
	"sort"

	"github.com/awcullen/opcua/ua"
	"github.com/djherbis/buffer"
)

var (
	host, _ = os.Hostname()
)

// Dial returns a secure channel to the OPC UA server with the given URL and options.
func Dial(ctx context.Context, endpointURL string, opts ...Option) (c *Client, err error) {

	cli := &Client{
		endpointURL:       endpointURL,
		userIdentity:      ua.AnonymousIdentity{},
		applicationName:   "application",
		sessionTimeout:    defaultSessionTimeout,
		securityPolicyURI: ua.SecurityPolicyURIBestAvailable,
		timeoutHint:       defaultTimeoutHint,
		diagnosticsHint:   defaultDiagnosticsHint,
		tokenLifetime:     defaultTokenRequestedLifetime,
		connectTimeout:    defaultConnectTimeout,
		maxBufferSize:     defaultMaxBufferSize,
		maxMessageSize:    defaultMaxMessageSize,
		maxChunkCount:     defaultMaxChunkCount,
		trace:             false,
		forcedEndpoint:    false,
	}

	// apply each option to the default
	for _, opt := range opts {
		if err := opt(cli); err != nil {
			return nil, err
		}
	}

	// get endpoints from discovery url
	req := &ua.GetEndpointsRequest{
		EndpointURL: endpointURL,
		ProfileURIs: []string{ua.TransportProfileURIUaTcpTransport},
	}
	res, err := GetEndpoints(ctx, req)
	if err != nil {
		return nil, err
	}

	// order endpoints by decreasing security level.
	var orderedEndpoints = res.Endpoints
	sort.Slice(orderedEndpoints, func(i, j int) bool {
		return orderedEndpoints[i].SecurityLevel < orderedEndpoints[j].SecurityLevel // first will be the "None" security policy if available
	})

	// if client certificate is not set then limit secuity policy to none
	securityPolicyURI := cli.securityPolicyURI
	securityMode := cli.securityMode
	if securityPolicyURI == ua.SecurityPolicyURIBestAvailable && len(cli.localCertificate) == 0 {
		securityPolicyURI = ua.SecurityPolicyURINone
		securityMode = ua.MessageSecurityModeNone
	}

	// select first endpoint with matching policy uri and security mode.
	var selectedEndpoint *ua.EndpointDescription
	for _, e := range orderedEndpoints {
		// filter out unsupported policy uri
		switch e.SecurityPolicyURI {
		case ua.SecurityPolicyURINone, ua.SecurityPolicyURIBasic128Rsa15,
			ua.SecurityPolicyURIBasic256, ua.SecurityPolicyURIBasic256Sha256,
			ua.SecurityPolicyURIAes128Sha256RsaOaep, ua.SecurityPolicyURIAes256Sha256RsaPss:
		default:
			continue
		}
		// if policy uri is a match
		if (securityPolicyURI == "" || e.SecurityPolicyURI == securityPolicyURI) &&
			(securityMode == ua.MessageSecurityModeInvalid || e.SecurityMode == securityMode) {
			selectedEndpoint = &e
			break
		}
	}
	if selectedEndpoint == nil {
		return nil, ua.BadSecurityModeRejected
	}
	if cli.forcedEndpoint {
		cli.endpointURL = endpointURL
	} else {
		cli.endpointURL = selectedEndpoint.EndpointURL
	}

	cli.securityPolicyURI = selectedEndpoint.SecurityPolicyURI
	cli.securityMode = selectedEndpoint.SecurityMode
	cli.serverCertificate = []byte(selectedEndpoint.ServerCertificate)
	cli.userTokenPolicies = selectedEndpoint.UserIdentityTokens

	cli.localDescription = ua.ApplicationDescription{
		ApplicationName: ua.LocalizedText{Text: cli.applicationName},
		ApplicationType: ua.ApplicationTypeClient,
		ApplicationURI:  fmt.Sprintf("urn:%s:%s", host, cli.applicationName),
	}

	if len(cli.localCertificate) > 0 {
		// if cert has URI then update local description
		if crts, err := x509.ParseCertificates(cli.localCertificate); err == nil && len(crts) > 0 {
			if len(crts[0].URIs) > 0 {
				cli.localDescription.ApplicationURI = crts[0].URIs[0].String()
			}
		}
	}

	cli.channel = newClientSecureChannel(
		cli.localDescription,
		cli.localCertificate,
		cli.localPrivateKey,
		cli.endpointURL,
		cli.securityPolicyURI,
		cli.securityMode,
		cli.serverCertificate,
		cli.connectTimeout,
		cli.trustedCertsPath,
		cli.trustedCRLsPath,
		cli.issuerCertsPath,
		cli.issuerCRLsPath,
		cli.rejectedCertsPath,
		cli.suppressHostNameInvalid,
		cli.suppressCertificateExpired,
		cli.suppressCertificateChainIncomplete,
		cli.suppressCertificateRevocationUnknown,
		cli.timeoutHint,
		cli.diagnosticsHint,
		cli.tokenLifetime,
		cli.maxBufferSize,
		cli.maxMessageSize,
		cli.maxChunkCount,
		cli.trace)

	// open session and read the namespace table
	if err := cli.open(ctx); err != nil {
		cli.Abort(ctx)
		return nil, err
	}

	return cli, nil
}

// Client for exchanging binary encoded requests and responses with an OPC UA server.
// Uses TCP with the binary security protocol UA-SecureConversation 1.0 and the binary message encoding UA-Binary 1.0.
type Client struct {
	channel                              *clientSecureChannel
	localDescription                     ua.ApplicationDescription
	endpointURL                          string
	securityPolicyURI                    string
	securityMode                         ua.MessageSecurityMode
	serverCertificate                    []byte
	userTokenPolicies                    []ua.UserTokenPolicy
	userIdentity                         any
	sessionID                            ua.NodeID
	sessionName                          string
	applicationName                      string
	sessionTimeout                       float64
	clientSignature                      ua.SignatureData
	identityToken                        any
	identityTokenSignature               ua.SignatureData
	timeoutHint                          uint32
	diagnosticsHint                      uint32
	tokenLifetime                        uint32
	localCertificate                     []byte
	localPrivateKey                      *rsa.PrivateKey
	trustedCertsPath                     string
	trustedCRLsPath                      string
	issuerCertsPath                      string
	issuerCRLsPath                       string
	rejectedCertsPath                    string
	suppressHostNameInvalid              bool
	suppressCertificateExpired           bool
	suppressCertificateChainIncomplete   bool
	suppressCertificateRevocationUnknown bool
	connectTimeout                       int64
	maxBufferSize                        uint32
	maxMessageSize                       uint32
	maxChunkCount                        uint32
	trace                                bool
	forcedEndpoint                       bool
}

// EndpointURL gets the EndpointURL of the server.
func (ch *Client) EndpointURL() string {
	return ch.endpointURL
}

// SecurityPolicyURI gets the SecurityPolicyURI of the secure channel.
func (ch *Client) SecurityPolicyURI() string {
	return ch.securityPolicyURI
}

// SecurityMode gets the MessageSecurityMode of the secure channel.
func (ch *Client) SecurityMode() ua.MessageSecurityMode {
	return ch.securityMode
}

// SessionID gets the id of the current session.
func (ch *Client) SessionID() ua.NodeID {
	return ch.sessionID
}

// SessionTimeout gets the maximum number of milliseconds that the session will remain open without activity.
func (ch *Client) SessionTimeout() float64 {
	return ch.sessionTimeout
}

// MaxRequestMessageSize gets the maximum size for the body of any request message. Zero equals no limit.
func (ch *Client) MaxRequestMessageSize() uint32 {
	return ch.channel.maxRequestMessageSize
}

// IsClosing returns true when the client is closing.
func (ch *Client) IsClosing() bool {
	return ch.channel.IsClosing()
}

// Request sends a service request to the server and returns the response.
func (ch *Client) request(ctx context.Context, req ua.ServiceRequest) (ua.ServiceResponse, error) {
	return ch.channel.Request(ctx, req)
}

// Open opens a secure channel to the server and creates a session.
func (ch *Client) open(ctx context.Context) error {
	if err := ch.channel.Open(ctx); err != nil {
		return err
	}

	var localNonce, localCertificate, remoteNonce []byte
	localNonce = getNextNonce(nonceLength)
	localCertificate = ch.channel.localCertificate

	var createSessionRequest = &ua.CreateSessionRequest{
		ClientDescription:       ch.localDescription,
		EndpointURL:             ch.endpointURL,
		SessionName:             ch.sessionName,
		ClientNonce:             ua.ByteString(localNonce),
		ClientCertificate:       ua.ByteString(localCertificate),
		RequestedSessionTimeout: ch.sessionTimeout,
		MaxResponseMessageSize:  defaultMaxMessageSize,
	}

	createSessionResponse, err := ch.createSession(ctx, createSessionRequest)
	if err != nil {
		return err
	}
	ch.sessionID = createSessionResponse.SessionID
	ch.channel.SetAuthenticationToken(createSessionResponse.AuthenticationToken)
	remoteNonce = []byte(createSessionResponse.ServerNonce)
	ch.sessionTimeout = createSessionResponse.RevisedSessionTimeout
	ch.channel.maxRequestMessageSize = createSessionResponse.MaxRequestMessageSize

	// verify the server's certificate is the same as the certificate from the selected endpoint.
	if !bytes.Equal(ch.serverCertificate, []byte(createSessionResponse.ServerCertificate)) {
		return ua.BadCertificateInvalid
	}

	// verify the server's signature.
	switch ch.securityPolicyURI {
	case ua.SecurityPolicyURIBasic128Rsa15, ua.SecurityPolicyURIBasic256:
		hash := crypto.SHA1.New()
		hash.Write(localCertificate)
		hash.Write(localNonce)
		hashed := hash.Sum(nil)
		err := rsa.VerifyPKCS1v15(ch.channel.remotePublicKey, crypto.SHA1, hashed, []byte(createSessionResponse.ServerSignature.Signature))
		if err != nil {
			return ua.BadApplicationSignatureInvalid
		}

	case ua.SecurityPolicyURIBasic256Sha256, ua.SecurityPolicyURIAes128Sha256RsaOaep:
		hash := crypto.SHA256.New()
		hash.Write(localCertificate)
		hash.Write(localNonce)
		hashed := hash.Sum(nil)
		err := rsa.VerifyPKCS1v15(ch.channel.remotePublicKey, crypto.SHA256, hashed, []byte(createSessionResponse.ServerSignature.Signature))
		if err != nil {
			return ua.BadApplicationSignatureInvalid
		}

	case ua.SecurityPolicyURIAes256Sha256RsaPss:
		hash := crypto.SHA256.New()
		hash.Write(localCertificate)
		hash.Write(localNonce)
		hashed := hash.Sum(nil)
		err := rsa.VerifyPSS(ch.channel.remotePublicKey, crypto.SHA256, hashed, []byte(createSessionResponse.ServerSignature.Signature), &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
		if err != nil {
			return ua.BadApplicationSignatureInvalid
		}
	}

	// create client signature
	var clientSignature ua.SignatureData
	switch ch.securityPolicyURI {
	case ua.SecurityPolicyURIBasic128Rsa15, ua.SecurityPolicyURIBasic256:
		hash := crypto.SHA1.New()
		hash.Write(ch.serverCertificate)
		hash.Write(remoteNonce)
		hashed := hash.Sum(nil)
		signature, err := rsa.SignPKCS1v15(rand.Reader, ch.channel.localPrivateKey, crypto.SHA1, hashed)
		if err != nil {
			return err
		}
		clientSignature = ua.SignatureData{
			Signature: ua.ByteString(signature),
			Algorithm: ua.RsaSha1Signature,
		}

	case ua.SecurityPolicyURIBasic256Sha256, ua.SecurityPolicyURIAes128Sha256RsaOaep:
		hash := crypto.SHA256.New()
		hash.Write(ch.serverCertificate)
		hash.Write(remoteNonce)
		hashed := hash.Sum(nil)
		signature, err := rsa.SignPKCS1v15(rand.Reader, ch.channel.localPrivateKey, crypto.SHA256, hashed)
		if err != nil {
			return err
		}
		clientSignature = ua.SignatureData{
			Signature: ua.ByteString(signature),
			Algorithm: ua.RsaSha256Signature,
		}

	case ua.SecurityPolicyURIAes256Sha256RsaPss:
		hash := crypto.SHA256.New()
		hash.Write(ch.serverCertificate)
		hash.Write(remoteNonce)
		hashed := hash.Sum(nil)
		signature, err := rsa.SignPSS(rand.Reader, ch.channel.localPrivateKey, crypto.SHA256, hashed, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
		if err != nil {
			return err
		}
		clientSignature = ua.SignatureData{
			Signature: ua.ByteString(signature),
			Algorithm: ua.RsaPssSha256Signature,
		}

	default:
		clientSignature = ua.SignatureData{}
	}

	// supported UserIdentityToken types are AnonymousIdentityToken, UserNameIdentityToken, IssuedIdentityToken, X509IdentityToken
	var identityToken any
	var identityTokenSignature ua.SignatureData
	switch ui := ch.userIdentity.(type) {

	case ua.IssuedIdentity:
		var tokenPolicy *ua.UserTokenPolicy
		for _, t := range ch.userTokenPolicies {
			if t.TokenType == ua.UserTokenTypeIssuedToken {
				tokenPolicy = &t
				break
			}
		}
		if tokenPolicy == nil {
			return ua.BadIdentityTokenRejected
		}

		secPolicyURI := tokenPolicy.SecurityPolicyURI
		if secPolicyURI == "" {
			secPolicyURI = ch.securityPolicyURI
		}

		switch secPolicyURI {
		case ua.SecurityPolicyURIBasic128Rsa15:
			publickey := ch.channel.remotePublicKey
			if publickey == nil {
				return ua.BadIdentityTokenRejected
			}
			plainBuf := buffer.NewPartitionAt(ch.channel.bufferPool)
			cipherBuf := buffer.NewPartitionAt(ch.channel.bufferPool)
			binary.Write(plainBuf, binary.LittleEndian, uint32(len(ui.TokenData)+len(remoteNonce)))
			plainBuf.Write([]byte(ui.TokenData))
			plainBuf.Write(remoteNonce)
			plainText := make([]byte, publickey.Size()-11)
			for plainBuf.Len() > 0 {
				plainBuf.Read(plainText)
				cipherText, err := rsa.EncryptPKCS1v15(rand.Reader, publickey, plainText)
				if err != nil {
					return err
				}
				cipherBuf.Write(cipherText)
			}
			cipherBytes := make([]byte, cipherBuf.Len())
			cipherBuf.Read(cipherBytes)
			plainBuf.Reset()
			cipherBuf.Reset()

			identityToken = ua.IssuedIdentityToken{
				TokenData:           ua.ByteString(cipherBytes),
				EncryptionAlgorithm: ua.RsaV15KeyWrap,
				PolicyID:            tokenPolicy.PolicyID,
			}
			identityTokenSignature = ua.SignatureData{}

		case ua.SecurityPolicyURIBasic256, ua.SecurityPolicyURIBasic256Sha256, ua.SecurityPolicyURIAes128Sha256RsaOaep:
			publickey := ch.channel.remotePublicKey
			if publickey == nil {
				return ua.BadIdentityTokenRejected
			}
			plainBuf := buffer.NewPartitionAt(ch.channel.bufferPool)
			cipherBuf := buffer.NewPartitionAt(ch.channel.bufferPool)
			binary.Write(plainBuf, binary.LittleEndian, uint32(len(ui.TokenData)+len(remoteNonce)))
			plainBuf.Write([]byte(ui.TokenData))
			plainBuf.Write(remoteNonce)
			plainText := make([]byte, publickey.Size()-42)
			for plainBuf.Len() > 0 {
				plainBuf.Read(plainText)
				cipherText, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, publickey, plainText, []byte{})
				if err != nil {
					return err
				}
				cipherBuf.Write(cipherText)
			}
			cipherBytes := make([]byte, cipherBuf.Len())
			cipherBuf.Read(cipherBytes)
			plainBuf.Reset()
			cipherBuf.Reset()

			identityToken = ua.IssuedIdentityToken{
				TokenData:           ua.ByteString(cipherBytes),
				EncryptionAlgorithm: ua.RsaOaepKeyWrap,
				PolicyID:            tokenPolicy.PolicyID,
			}
			identityTokenSignature = ua.SignatureData{}

		case ua.SecurityPolicyURIAes256Sha256RsaPss:
			publickey := ch.channel.remotePublicKey
			if publickey == nil {
				return ua.BadIdentityTokenRejected
			}
			plainBuf := buffer.NewPartitionAt(ch.channel.bufferPool)
			cipherBuf := buffer.NewPartitionAt(ch.channel.bufferPool)
			binary.Write(plainBuf, binary.LittleEndian, uint32(len(ui.TokenData)+len(remoteNonce)))
			plainBuf.Write([]byte(ui.TokenData))
			plainBuf.Write(remoteNonce)
			plainText := make([]byte, publickey.Size()-66)
			for plainBuf.Len() > 0 {
				plainBuf.Read(plainText)
				cipherText, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, publickey, plainText, []byte{})
				if err != nil {
					return err
				}
				cipherBuf.Write(cipherText)
			}
			cipherBytes := make([]byte, cipherBuf.Len())
			cipherBuf.Read(cipherBytes)
			plainBuf.Reset()
			cipherBuf.Reset()

			identityToken = ua.IssuedIdentityToken{
				TokenData:           ua.ByteString(cipherBytes),
				EncryptionAlgorithm: ua.RsaOaepSha256KeyWrap,
				PolicyID:            tokenPolicy.PolicyID,
			}
			identityTokenSignature = ua.SignatureData{}

		default:
			identityToken = ua.IssuedIdentityToken{
				TokenData:           ui.TokenData,
				EncryptionAlgorithm: "",
				PolicyID:            tokenPolicy.PolicyID,
			}
			identityTokenSignature = ua.SignatureData{}
		}

	case ua.X509Identity:
		var tokenPolicy *ua.UserTokenPolicy
		for _, t := range ch.userTokenPolicies {
			if t.TokenType == ua.UserTokenTypeCertificate {
				tokenPolicy = &t
				break
			}
		}
		if tokenPolicy == nil {
			return ua.BadIdentityTokenRejected
		}

		secPolicyURI := tokenPolicy.SecurityPolicyURI
		if secPolicyURI == "" {
			secPolicyURI = ch.securityPolicyURI
		}

		switch secPolicyURI {
		case ua.SecurityPolicyURIBasic128Rsa15, ua.SecurityPolicyURIBasic256:
			hash := crypto.SHA1.New()
			hash.Write(ch.serverCertificate)
			hash.Write(remoteNonce)
			hashed := hash.Sum(nil)
			signature, err := rsa.SignPKCS1v15(rand.Reader, ui.Key, crypto.SHA1, hashed)
			if err != nil {
				return err
			}
			identityToken = ua.X509IdentityToken{
				CertificateData: ui.Certificate,
				PolicyID:        tokenPolicy.PolicyID,
			}
			identityTokenSignature = ua.SignatureData{
				Signature: ua.ByteString(signature),
				Algorithm: ua.RsaSha1Signature,
			}

		case ua.SecurityPolicyURIBasic256Sha256, ua.SecurityPolicyURIAes128Sha256RsaOaep:
			hash := crypto.SHA256.New()
			hash.Write(ch.serverCertificate)
			hash.Write(remoteNonce)
			hashed := hash.Sum(nil)
			signature, err := rsa.SignPKCS1v15(rand.Reader, ui.Key, crypto.SHA256, hashed)
			if err != nil {
				return err
			}
			identityToken = ua.X509IdentityToken{
				CertificateData: ui.Certificate,
				PolicyID:        tokenPolicy.PolicyID,
			}
			identityTokenSignature = ua.SignatureData{
				Signature: ua.ByteString(signature),
				Algorithm: ua.RsaSha256Signature,
			}

		case ua.SecurityPolicyURIAes256Sha256RsaPss:
			hash := crypto.SHA256.New()
			hash.Write(ch.serverCertificate)
			hash.Write(remoteNonce)
			hashed := hash.Sum(nil)
			signature, err := rsa.SignPSS(rand.Reader, ui.Key, crypto.SHA256, hashed, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
			if err != nil {
				return err
			}
			identityToken = ua.X509IdentityToken{
				CertificateData: ui.Certificate,
				PolicyID:        tokenPolicy.PolicyID,
			}
			identityTokenSignature = ua.SignatureData{
				Signature: ua.ByteString(signature),
				Algorithm: ua.RsaPssSha256Signature,
			}

		default:
			identityToken = ua.X509IdentityToken{
				CertificateData: ui.Certificate,
				PolicyID:        tokenPolicy.PolicyID,
			}
			identityTokenSignature = ua.SignatureData{}
		}

	case ua.UserNameIdentity:
		var tokenPolicy *ua.UserTokenPolicy
		for _, t := range ch.userTokenPolicies {
			if t.TokenType == ua.UserTokenTypeUserName {
				tokenPolicy = &t
				break
			}
		}
		if tokenPolicy == nil {
			return ua.BadIdentityTokenRejected
		}

		passwordBytes := []byte(ui.Password)
		secPolicyURI := tokenPolicy.SecurityPolicyURI
		if secPolicyURI == "" {
			secPolicyURI = ch.securityPolicyURI
		}

		switch secPolicyURI {
		case ua.SecurityPolicyURIBasic128Rsa15:
			publickey := ch.channel.remotePublicKey
			if publickey == nil {
				return ua.BadIdentityTokenRejected
			}
			plainBuf := buffer.NewPartitionAt(ch.channel.bufferPool)
			cipherBuf := buffer.NewPartitionAt(ch.channel.bufferPool)
			binary.Write(plainBuf, binary.LittleEndian, uint32(len(passwordBytes)+len(remoteNonce)))
			plainBuf.Write(passwordBytes)
			plainBuf.Write(remoteNonce)
			plainText := make([]byte, publickey.Size()-11)
			for plainBuf.Len() > 0 {
				plainBuf.Read(plainText)
				// encrypt with remote public key.
				cipherText, err := rsa.EncryptPKCS1v15(rand.Reader, publickey, plainText)
				if err != nil {
					return err
				}
				cipherBuf.Write(cipherText)
			}
			cipherBytes := make([]byte, cipherBuf.Len())
			cipherBuf.Read(cipherBytes)
			plainBuf.Reset()
			cipherBuf.Reset()

			identityToken = ua.UserNameIdentityToken{
				UserName:            ui.UserName,
				Password:            ua.ByteString(cipherBytes),
				EncryptionAlgorithm: ua.RsaV15KeyWrap,
				PolicyID:            tokenPolicy.PolicyID,
			}
			identityTokenSignature = ua.SignatureData{}

		case ua.SecurityPolicyURIBasic256, ua.SecurityPolicyURIBasic256Sha256, ua.SecurityPolicyURIAes128Sha256RsaOaep:
			publickey := ch.channel.remotePublicKey
			if publickey == nil {
				return ua.BadIdentityTokenRejected
			}
			plainBuf := buffer.NewPartitionAt(ch.channel.bufferPool)
			cipherBuf := buffer.NewPartitionAt(ch.channel.bufferPool)
			binary.Write(plainBuf, binary.LittleEndian, uint32(len(passwordBytes)+len(remoteNonce)))
			plainBuf.Write(passwordBytes)
			plainBuf.Write(remoteNonce)
			plainText := make([]byte, publickey.Size()-42)
			for plainBuf.Len() > 0 {
				plainBuf.Read(plainText)
				// encrypt with remote public key.
				cipherText, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, publickey, plainText, []byte{})
				if err != nil {
					return err
				}
				cipherBuf.Write(cipherText)
			}
			cipherBytes := make([]byte, cipherBuf.Len())
			cipherBuf.Read(cipherBytes)
			plainBuf.Reset()
			cipherBuf.Reset()

			identityToken = ua.UserNameIdentityToken{
				UserName:            ui.UserName,
				Password:            ua.ByteString(cipherBytes),
				EncryptionAlgorithm: ua.RsaOaepKeyWrap,
				PolicyID:            tokenPolicy.PolicyID,
			}
			identityTokenSignature = ua.SignatureData{}

		case ua.SecurityPolicyURIAes256Sha256RsaPss:
			publickey := ch.channel.remotePublicKey
			if publickey == nil {
				return ua.BadIdentityTokenRejected
			}
			plainBuf := buffer.NewPartitionAt(ch.channel.bufferPool)
			cipherBuf := buffer.NewPartitionAt(ch.channel.bufferPool)
			binary.Write(plainBuf, binary.LittleEndian, uint32(len(passwordBytes)+len(remoteNonce)))
			plainBuf.Write(passwordBytes)
			plainBuf.Write(remoteNonce)
			plainText := make([]byte, publickey.Size()-66)
			for plainBuf.Len() > 0 {
				plainBuf.Read(plainText)
				// encrypt with remote public key.
				cipherText, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, publickey, plainText, []byte{})
				if err != nil {
					return err
				}
				cipherBuf.Write(cipherText)
			}
			cipherBytes := make([]byte, cipherBuf.Len())
			cipherBuf.Read(cipherBytes)
			plainBuf.Reset()
			cipherBuf.Reset()

			identityToken = ua.UserNameIdentityToken{
				UserName:            ui.UserName,
				Password:            ua.ByteString(cipherBytes),
				EncryptionAlgorithm: ua.RsaOaepSha256KeyWrap,
				PolicyID:            tokenPolicy.PolicyID,
			}
			identityTokenSignature = ua.SignatureData{}

		default:
			identityToken = ua.UserNameIdentityToken{
				UserName:            ui.UserName,
				Password:            ua.ByteString(passwordBytes),
				EncryptionAlgorithm: "",
				PolicyID:            tokenPolicy.PolicyID,
			}
			identityTokenSignature = ua.SignatureData{}
		}

	default:
		var tokenPolicy *ua.UserTokenPolicy
		for _, t := range ch.userTokenPolicies {
			if t.TokenType == ua.UserTokenTypeAnonymous {
				tokenPolicy = &t
				break
			}
		}
		if tokenPolicy == nil {
			return ua.BadIdentityTokenRejected
		}

		identityToken = ua.AnonymousIdentityToken{PolicyID: tokenPolicy.PolicyID}
		identityTokenSignature = ua.SignatureData{}
	}

	// save for re-connect (instead of remote nonce)
	ch.clientSignature = clientSignature
	ch.identityToken = identityToken
	ch.identityTokenSignature = identityTokenSignature

	activateSessionRequest := &ua.ActivateSessionRequest{
		ClientSignature:    ch.clientSignature,
		LocaleIDs:          []string{"en"},
		UserIdentityToken:  identityToken,
		UserTokenSignature: ch.identityTokenSignature,
	}
	activateSessionResponse, err := ch.activateSession(ctx, activateSessionRequest)
	if err != nil {
		return err
	}
	_ = []byte(activateSessionResponse.ServerNonce)

	// fetch namespace array, etc.
	var readRequest = &ua.ReadRequest{
		NodesToRead: []ua.ReadValueID{
			{
				NodeID:      ua.VariableIDServerNamespaceArray,
				AttributeID: ua.AttributeIDValue,
			},
			{
				NodeID:      ua.VariableIDServerServerArray,
				AttributeID: ua.AttributeIDValue,
			},
		},
	}
	readResponse, err := ch.Read(ctx, readRequest)
	if err != nil {
		return err
	}
	if len(readResponse.Results) == 2 {
		if readResponse.Results[0].StatusCode.IsGood() {
			value := readResponse.Results[0].Value.([]string)
			ch.channel.SetNamespaceURIs(value)
		}

		if readResponse.Results[1].StatusCode.IsGood() {
			value := readResponse.Results[1].Value.([]string)
			ch.channel.SetServerURIs(value)
		}
	}
	return nil
}

// Close closes the session and secure channel.
func (ch *Client) Close(ctx context.Context) error {
	var request = &ua.CloseSessionRequest{
		DeleteSubscriptions: true,
	}
	_, err := ch.closeSession(ctx, request)
	if err != nil {
		return err
	}
	ch.channel.Close(ctx)
	return nil
}

// Close closes the session and secure channel.
func (ch *Client) CloseDeleteSubscriptions(ctx context.Context, deleteSubscriptions bool) error {
	var request = &ua.CloseSessionRequest{
		DeleteSubscriptions: deleteSubscriptions,
	}
	_, err := ch.closeSession(ctx, request)
	if err != nil {
		return err
	}
	ch.channel.Close(ctx)
	return nil
}

// Abort closes the client abruptly.
func (ch *Client) Abort(ctx context.Context) error {
	ch.channel.Abort(ctx)
	return nil
}

func (ch *Client) GetNamespaceURIs() []string {
	return ch.channel.NamespaceURIs()
}
