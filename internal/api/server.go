package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	core "github.com/iden3/go-iden3-core/v2"
	"github.com/iden3/go-iden3-core/v2/w3c"
	"github.com/iden3/go-schema-processor/v2/verifiable"
	"github.com/iden3/iden3comm/v2"
	"github.com/iden3/iden3comm/v2/packers"
	"github.com/iden3/iden3comm/v2/protocol"

	"github.com/polygonid/sh-id-platform/internal/common"
	"github.com/polygonid/sh-id-platform/internal/config"
	"github.com/polygonid/sh-id-platform/internal/core/domain"
	"github.com/polygonid/sh-id-platform/internal/core/ports"
	"github.com/polygonid/sh-id-platform/internal/core/services"
	"github.com/polygonid/sh-id-platform/internal/gateways"
	"github.com/polygonid/sh-id-platform/internal/health"
	"github.com/polygonid/sh-id-platform/internal/kms"
	"github.com/polygonid/sh-id-platform/internal/log"
	"github.com/polygonid/sh-id-platform/internal/repositories"
	"github.com/polygonid/sh-id-platform/pkg/network"
	"github.com/polygonid/sh-id-platform/pkg/schema"
)

// Server implements StrictServerInterface and holds the implementation of all API controllers
// This is the glue to the API autogenerated code
type Server struct {
	cfg              *config.Configuration
	identityService  ports.IdentityService
	claimService     ports.ClaimsService
	qrService        ports.QrStoreService
	publisherGateway ports.Publisher
	packageManager   *iden3comm.PackageManager
	health           *health.Status
	accountService   ports.AccountService
	networkResolver  network.Resolver
}

// NewServer is a Server constructor
func NewServer(cfg *config.Configuration, identityService ports.IdentityService, accountService ports.AccountService, claimsService ports.ClaimsService, qrService ports.QrStoreService, publisherGateway ports.Publisher, packageManager *iden3comm.PackageManager, networkResolver network.Resolver, health *health.Status) *Server {
	return &Server{
		cfg:              cfg,
		identityService:  identityService,
		claimService:     claimsService,
		qrService:        qrService,
		publisherGateway: publisherGateway,
		packageManager:   packageManager,
		health:           health,
		accountService:   accountService,
		networkResolver:  networkResolver,
	}
}

// Health is a method
func (s *Server) Health(_ context.Context, _ HealthRequestObject) (HealthResponseObject, error) {
	var resp Health200JSONResponse = s.health.Status()

	return resp, nil
}

// GetDocumentation this method will be overridden in the main function
func (s *Server) GetDocumentation(_ context.Context, _ GetDocumentationRequestObject) (GetDocumentationResponseObject, error) {
	return nil, nil
}

// GetFavicon this method will be overridden in the main function
func (s *Server) GetFavicon(_ context.Context, _ GetFaviconRequestObject) (GetFaviconResponseObject, error) {
	return nil, nil
}

// GetYaml this method will be overridden in the main function
func (s *Server) GetYaml(_ context.Context, _ GetYamlRequestObject) (GetYamlResponseObject, error) {
	return nil, nil
}

// CreateIdentity is created identity controller
func (s *Server) CreateIdentity(ctx context.Context, request CreateIdentityRequestObject) (CreateIdentityResponseObject, error) {
	method := request.Body.DidMetadata.Method
	blockchain := request.Body.DidMetadata.Blockchain
	network := request.Body.DidMetadata.Network
	keyType := request.Body.DidMetadata.Type

	if keyType != "BJJ" && keyType != "ETH" {
		return CreateIdentity400JSONResponse{
			N400JSONResponse{
				Message: "Type must be BJJ or ETH",
			},
		}, nil
	}

	resolverPrefix := fmt.Sprintf("%s:%s", blockchain, network)
	rhsSettings, err := s.networkResolver.GetRhsSettings(resolverPrefix)
	if err != nil {
		return CreateIdentity400JSONResponse{N400JSONResponse{Message: "error getting reverse hash service settings"}}, nil
	}

	identity, err := s.identityService.Create(ctx, s.cfg.ServerUrl, &ports.DIDCreationOptions{
		Method:                  core.DIDMethod(method),
		Network:                 core.NetworkID(network),
		Blockchain:              core.Blockchain(blockchain),
		KeyType:                 kms.KeyType(keyType),
		AuthBJJCredentialStatus: verifiable.CredentialStatusType(rhsSettings.CredentialStatusType),
	})
	if err != nil {
		if errors.Is(err, services.ErrWrongDIDMetada) {
			return CreateIdentity400JSONResponse{
				N400JSONResponse{
					Message: err.Error(),
				},
			}, nil
		}

		if errors.Is(err, kms.ErrPermissionDenied) {
			var message string
			if s.cfg.VaultUserPassAuthEnabled {
				message = "Issuer Node cannot connect with Vault. Please check the value of ISSUER_VAULT_USERPASS_AUTH_PASSWORD variable."
			} else {
				message = `Issuer Node cannot connect with Vault. Please check the value of ISSUER_KEY_STORE_TOKEN variable.`
			}

			log.Info(ctx, message+". More information in this link: https://devs.polygonid.com/docs/issuer/vault-auth")
			return CreateIdentity403JSONResponse{
				N403JSONResponse{
					Message: message,
				},
			}, nil
		}

		return nil, err
	}

	return CreateIdentity201JSONResponse{
		Identifier: &identity.Identifier,
		State: &IdentityState{
			BlockNumber:        identity.State.BlockNumber,
			BlockTimestamp:     identity.State.BlockTimestamp,
			ClaimsTreeRoot:     identity.State.ClaimsTreeRoot,
			CreatedAt:          TimeUTC(identity.State.CreatedAt),
			ModifiedAt:         TimeUTC(identity.State.ModifiedAt),
			PreviousState:      identity.State.PreviousState,
			RevocationTreeRoot: identity.State.RevocationTreeRoot,
			RootOfRoots:        identity.State.RootOfRoots,
			State:              identity.State.State,
			Status:             string(identity.State.Status),
			TxID:               identity.State.TxID,
		},
		Address: identity.Address,
	}, nil
}

// CreateClaim is claim creation controller
func (s *Server) CreateClaim(ctx context.Context, request CreateClaimRequestObject) (CreateClaimResponseObject, error) {
	did, err := w3c.ParseDID(request.Identifier)
	if err != nil {
		return CreateClaim400JSONResponse{N400JSONResponse{Message: err.Error()}}, nil
	}
	var expiration *time.Time
	if request.Body.Expiration != nil {
		expiration = common.ToPointer(time.Unix(*request.Body.Expiration, 0))
	}

	resolverPrefix, err := common.ResolverPrefix(did)
	if err != nil {
		return CreateClaim400JSONResponse{N400JSONResponse{Message: "error parsing did"}}, nil
	}

	rhsSettings, err := s.networkResolver.GetRhsSettings(resolverPrefix)
	if err != nil {
		return CreateClaim400JSONResponse{N400JSONResponse{Message: "error getting reverse hash service settings"}}, nil
	}
	req := ports.NewCreateClaimRequest(did, request.Body.CredentialSchema, request.Body.CredentialSubject, expiration, request.Body.Type, request.Body.Version, request.Body.SubjectPosition, request.Body.MerklizedRootPosition, common.ToPointer(true), common.ToPointer(true), nil, false, verifiable.CredentialStatusType(rhsSettings.CredentialStatusType), toVerifiableRefreshService(request.Body.RefreshService), request.Body.RevNonce)

	resp, err := s.claimService.Save(ctx, req)
	if err != nil {
		if errors.Is(err, services.ErrJSONLdContext) {
			return CreateClaim400JSONResponse{N400JSONResponse{Message: err.Error()}}, nil
		}
		if errors.Is(err, services.ErrProcessSchema) {
			return CreateClaim400JSONResponse{N400JSONResponse{Message: err.Error()}}, nil
		}
		if errors.Is(err, services.ErrLoadingSchema) {
			return CreateClaim422JSONResponse{N422JSONResponse{Message: err.Error()}}, nil
		}
		if errors.Is(err, services.ErrMalformedURL) {
			return CreateClaim400JSONResponse{N400JSONResponse{Message: err.Error()}}, nil
		}
		if errors.Is(err, services.ErrParseClaim) {
			return CreateClaim400JSONResponse{N400JSONResponse{Message: err.Error()}}, nil
		}
		if errors.Is(err, services.ErrInvalidCredentialSubject) {
			return CreateClaim400JSONResponse{N400JSONResponse{Message: err.Error()}}, nil
		}
		if errors.Is(err, services.ErrLoadingSchema) {
			return CreateClaim400JSONResponse{N400JSONResponse{Message: err.Error()}}, nil
		}
		if errors.Is(err, services.ErrAssigningMTPProof) {
			return CreateClaim400JSONResponse{N400JSONResponse{Message: err.Error()}}, nil
		}
		if errors.Is(err, services.ErrUnsupportedRefreshServiceType) {
			return CreateClaim400JSONResponse{N400JSONResponse{Message: err.Error()}}, nil
		}
		if errors.Is(err, services.ErrRefreshServiceLacksExpirationTime) {
			return CreateClaim400JSONResponse{N400JSONResponse{Message: err.Error()}}, nil
		}
		return CreateClaim500JSONResponse{N500JSONResponse{Message: err.Error()}}, nil
	}
	return CreateClaim201JSONResponse{Id: resp.ID.String()}, nil
}

// RevokeClaim is the revocation claim controller
func (s *Server) RevokeClaim(ctx context.Context, request RevokeClaimRequestObject) (RevokeClaimResponseObject, error) {
	did, err := w3c.ParseDID(request.Identifier)
	if err != nil {
		log.Warn(ctx, "revoke claim invalid did", "err", err, "req", request)
		return RevokeClaim400JSONResponse{N400JSONResponse{err.Error()}}, nil
	}

	if err := s.claimService.Revoke(ctx, *did, uint64(request.Nonce), ""); err != nil {
		if errors.Is(err, repositories.ErrClaimDoesNotExist) {
			return RevokeClaim404JSONResponse{N404JSONResponse{
				Message: "the claim does not exist",
			}}, nil
		}

		return RevokeClaim500JSONResponse{N500JSONResponse{Message: err.Error()}}, nil
	}
	return RevokeClaim202JSONResponse{
		Message: "claim revocation request sent",
	}, nil
}

// GetRevocationStatus is the controller to get revocation status
func (s *Server) GetRevocationStatus(ctx context.Context, request GetRevocationStatusRequestObject) (GetRevocationStatusResponseObject, error) {
	issuerDID, err := w3c.ParseDID(request.Identifier)
	if err != nil {
		return GetRevocationStatus500JSONResponse{N500JSONResponse{
			Message: err.Error(),
		}}, nil
	}

	rs, err := s.claimService.GetRevocationStatus(ctx, *issuerDID, uint64(request.Nonce))
	if err != nil {
		return GetRevocationStatus500JSONResponse{N500JSONResponse{
			Message: err.Error(),
		}}, nil
	}

	response := GetRevocationStatus200JSONResponse{}
	response.Issuer.State = rs.Issuer.State
	response.Issuer.RevocationTreeRoot = rs.Issuer.RevocationTreeRoot
	response.Issuer.RootOfRoots = rs.Issuer.RootOfRoots
	response.Issuer.ClaimsTreeRoot = rs.Issuer.ClaimsTreeRoot
	response.Mtp.Existence = rs.MTP.Existence

	if rs.MTP.NodeAux != nil {
		key := rs.MTP.NodeAux.Key
		decodedKey := key.BigInt().String()
		value := rs.MTP.NodeAux.Value
		decodedValue := value.BigInt().String()
		response.Mtp.NodeAux = &struct {
			Key   *string `json:"key,omitempty"`
			Value *string `json:"value,omitempty"`
		}{
			Key:   &decodedKey,
			Value: &decodedValue,
		}
	}

	response.Mtp.Existence = rs.MTP.Existence
	siblings := make([]string, 0)
	for _, s := range rs.MTP.AllSiblings() {
		siblings = append(siblings, s.BigInt().String())
	}
	response.Mtp.Siblings = &siblings

	return response, err
}

// GetClaim is the controller to get a client.
func (s *Server) GetClaim(ctx context.Context, request GetClaimRequestObject) (GetClaimResponseObject, error) {
	if request.Identifier == "" {
		return GetClaim400JSONResponse{N400JSONResponse{"invalid did, cannot be empty"}}, nil
	}

	did, err := w3c.ParseDID(request.Identifier)
	if err != nil {
		return GetClaim400JSONResponse{N400JSONResponse{"invalid did"}}, nil
	}

	if request.Id == "" {
		return GetClaim400JSONResponse{N400JSONResponse{"cannot proceed with an empty claim id"}}, nil
	}

	clID, err := uuid.Parse(request.Id)
	if err != nil {
		return GetClaim400JSONResponse{N400JSONResponse{"invalid claim id"}}, nil
	}

	claim, err := s.claimService.GetByID(ctx, did, clID)
	if err != nil {
		if errors.Is(err, services.ErrClaimNotFound) {
			return GetClaim404JSONResponse{N404JSONResponse{err.Error()}}, nil
		}
		return GetClaim500JSONResponse{N500JSONResponse{err.Error()}}, nil
	}

	w3c, err := schema.FromClaimModelToW3CCredential(*claim)
	if err != nil {
		return GetClaim500JSONResponse{N500JSONResponse{"invalid claim format"}}, nil
	}

	return GetClaim200JSONResponse(toGetClaim200Response(w3c)), nil
}

// GetClaims is the controller to get multiple claims of a determined identity
func (s *Server) GetClaims(ctx context.Context, request GetClaimsRequestObject) (GetClaimsResponseObject, error) {
	if request.Identifier == "" {
		return GetClaims400JSONResponse{N400JSONResponse{"invalid did, cannot be empty"}}, nil
	}

	did, err := w3c.ParseDID(request.Identifier)
	if err != nil {
		return GetClaims400JSONResponse{N400JSONResponse{"invalid did"}}, nil
	}

	filter, err := ports.NewClaimsFilter(
		request.Params.SchemaHash,
		request.Params.SchemaType,
		request.Params.Subject,
		request.Params.QueryField,
		request.Params.QueryValue,
		request.Params.Self,
		request.Params.Revoked)
	if err != nil {
		return GetClaims400JSONResponse{N400JSONResponse{err.Error()}}, nil
	}

	claims, _, err := s.claimService.GetAll(ctx, *did, filter)
	if err != nil && !errors.Is(err, services.ErrClaimNotFound) {
		return GetClaims500JSONResponse{N500JSONResponse{"there was an internal error trying to retrieve claims for the requested identifier"}}, nil
	}

	w3Claims, err := schema.FromClaimsModelToW3CCredential(claims)
	if err != nil {
		return GetClaims500JSONResponse{N500JSONResponse{"there was an internal error parsing the claims"}}, nil
	}

	return toGetClaims200Response(w3Claims), nil
}

// GetClaimQrCode returns a GetClaimQrCodeResponseObject that can be used with any QR generator to create a QR and
// scan it with polygon wallet to accept the claim
// TODO: this should be converted to a QR link
func (s *Server) GetClaimQrCode(ctx context.Context, request GetClaimQrCodeRequestObject) (GetClaimQrCodeResponseObject, error) {
	if request.Identifier == "" {
		return GetClaimQrCode400JSONResponse{N400JSONResponse{"invalid did, cannot be empty"}}, nil
	}

	did, err := w3c.ParseDID(request.Identifier)
	if err != nil {
		return GetClaimQrCode400JSONResponse{N400JSONResponse{"invalid did"}}, nil
	}

	if request.Id == "" {
		return GetClaimQrCode400JSONResponse{N400JSONResponse{"cannot proceed with an empty claim id"}}, nil
	}

	claimID, err := uuid.Parse(request.Id)
	if err != nil {
		return GetClaimQrCode400JSONResponse{N400JSONResponse{"invalid claim id"}}, nil
	}

	claim, err := s.claimService.GetByID(ctx, did, claimID)
	if err != nil {
		if errors.Is(err, services.ErrClaimNotFound) {
			return GetClaimQrCode404JSONResponse{N404JSONResponse{err.Error()}}, nil
		}
		return GetClaimQrCode500JSONResponse{N500JSONResponse{err.Error()}}, nil
	}
	return toGetClaimQrCode200JSONResponse(claim, s.cfg.ServerUrl), nil
}

// GetIdentities is the controller to get identities
func (s *Server) GetIdentities(ctx context.Context, request GetIdentitiesRequestObject) (GetIdentitiesResponseObject, error) {
	var response GetIdentities200JSONResponse
	var err error
	response, err = s.identityService.Get(ctx)
	if err != nil {
		return GetIdentities500JSONResponse{N500JSONResponse{
			Message: err.Error(),
		}}, nil
	}

	return response, nil
}

// Agent is the controller to fetch credentials from mobile
func (s *Server) Agent(ctx context.Context, request AgentRequestObject) (AgentResponseObject, error) {
	if request.Body == nil || *request.Body == "" {
		log.Debug(ctx, "agent empty request")
		return Agent400JSONResponse{N400JSONResponse{"cannot proceed with an empty request"}}, nil
	}
	basicMessage, err := s.packageManager.UnpackWithType(packers.MediaTypeZKPMessage, []byte(*request.Body))
	if err != nil {
		log.Debug(ctx, "agent bad request", "err", err, "body", *request.Body)
		return Agent400JSONResponse{N400JSONResponse{"cannot proceed with the given request"}}, nil
	}

	req, err := ports.NewAgentRequest(basicMessage)
	if err != nil {
		log.Error(ctx, "agent parsing request", "err", err)
		return Agent400JSONResponse{N400JSONResponse{err.Error()}}, nil
	}

	agent, err := s.claimService.Agent(ctx, req)
	if err != nil {
		log.Error(ctx, "agent error", "err", err)
		return Agent400JSONResponse{N400JSONResponse{err.Error()}}, nil
	}
	return Agent200JSONResponse{
		Body:     agent.Body,
		From:     agent.From,
		Id:       agent.ID,
		ThreadID: agent.ThreadID,
		To:       agent.To,
		Typ:      string(agent.Typ),
		Type:     string(agent.Type),
	}, nil
}

// PublishIdentityState - publish identity state on chain
func (s *Server) PublishIdentityState(ctx context.Context, request PublishIdentityStateRequestObject) (PublishIdentityStateResponseObject, error) {
	did, err := w3c.ParseDID(request.Identifier)
	if err != nil {
		return PublishIdentityState400JSONResponse{N400JSONResponse{"invalid did"}}, nil
	}

	publishedState, err := s.publisherGateway.PublishState(ctx, did)
	if err != nil {
		if errors.Is(err, gateways.ErrNoStatesToProcess) || errors.Is(err, gateways.ErrStateIsBeingProcessed) {
			return PublishIdentityState200JSONResponse{Message: err.Error()}, nil
		}
		return PublishIdentityState500JSONResponse{N500JSONResponse{err.Error()}}, nil
	}

	return PublishIdentityState202JSONResponse{
		ClaimsTreeRoot:     publishedState.ClaimsTreeRoot,
		RevocationTreeRoot: publishedState.RevocationTreeRoot,
		RootOfRoots:        publishedState.RootOfRoots,
		State:              publishedState.State,
		TxID:               publishedState.TxID,
	}, nil
}

// RetryPublishState - retry to publish the current state if it failed previously.
func (s *Server) RetryPublishState(ctx context.Context, request RetryPublishStateRequestObject) (RetryPublishStateResponseObject, error) {
	did, err := w3c.ParseDID(request.Identifier)
	if err != nil {
		return RetryPublishState400JSONResponse{N400JSONResponse{"invalid did"}}, nil
	}

	publishedState, err := s.publisherGateway.RetryPublishState(ctx, did)
	if err != nil {
		log.Error(ctx, "error retrying the publishing the state", "err", err)
		if errors.Is(err, gateways.ErrStateIsBeingProcessed) || errors.Is(err, gateways.ErrNoFailedStatesToProcess) {
			return RetryPublishState400JSONResponse{N400JSONResponse{Message: err.Error()}}, nil
		}
		return RetryPublishState500JSONResponse{N500JSONResponse{Message: err.Error()}}, nil
	}
	return RetryPublishState202JSONResponse{
		ClaimsTreeRoot:     publishedState.ClaimsTreeRoot,
		RevocationTreeRoot: publishedState.RevocationTreeRoot,
		RootOfRoots:        publishedState.RootOfRoots,
		State:              publishedState.State,
		TxID:               publishedState.TxID,
	}, nil
}

// GetQrFromStore is the controller to get qr bodies
func (s *Server) GetQrFromStore(ctx context.Context, request GetQrFromStoreRequestObject) (GetQrFromStoreResponseObject, error) {
	if request.Params.Id == nil {
		log.Warn(ctx, "qr store. Missing id parameter")
		return GetQrFromStore400JSONResponse{N400JSONResponse{"id is required"}}, nil
	}
	body, err := s.qrService.Find(ctx, *request.Params.Id)
	if err != nil {
		log.Error(ctx, "qr store. Finding qr", "err", err, "id", *request.Params.Id)
		return GetQrFromStore500JSONResponse{N500JSONResponse{"error looking for qr body"}}, nil
	}
	return NewQrContentResponse(body), nil
}

// GetIdentityDetails is the controller to get identity details
func (s *Server) GetIdentityDetails(ctx context.Context, request GetIdentityDetailsRequestObject) (GetIdentityDetailsResponseObject, error) {
	userDID, err := w3c.ParseDID(request.Identifier)
	if err != nil {
		log.Error(ctx, "get identity details. Parsing did", "err", err)
		return GetIdentityDetails400JSONResponse{
			N400JSONResponse{
				Message: "invalid did",
			},
		}, err
	}

	identity, err := s.identityService.GetByDID(ctx, *userDID)
	if err != nil {
		log.Error(ctx, "get identity details. Getting identity", "err", err)
		return GetIdentityDetails500JSONResponse{
			N500JSONResponse{
				Message: err.Error(),
			},
		}, err
	}

	if identity.KeyType == string(kms.KeyTypeEthereum) {
		did, err := w3c.ParseDID(identity.Identifier)
		if err != nil {
			log.Error(ctx, "get identity details. Parsing did", "err", err)
			return GetIdentityDetails400JSONResponse{N400JSONResponse{Message: "invalid did"}}, nil
		}
		balance, err := s.accountService.GetBalanceByDID(ctx, did)
		if err != nil {
			log.Error(ctx, "get identity details. Getting balance", "err", err)
			return GetIdentityDetails500JSONResponse{N500JSONResponse{Message: err.Error()}}, nil
		}
		identity.Balance = balance
	}

	response := GetIdentityDetails200JSONResponse{
		Identifier: &identity.Identifier,
		State: &IdentityState{
			BlockNumber:        identity.State.BlockNumber,
			BlockTimestamp:     identity.State.BlockTimestamp,
			ClaimsTreeRoot:     identity.State.ClaimsTreeRoot,
			CreatedAt:          TimeUTC(identity.State.CreatedAt),
			ModifiedAt:         TimeUTC(identity.State.ModifiedAt),
			PreviousState:      identity.State.PreviousState,
			RevocationTreeRoot: identity.State.RevocationTreeRoot,
			RootOfRoots:        identity.State.RootOfRoots,
			State:              identity.State.State,
			Status:             string(identity.State.Status),
			TxID:               identity.State.TxID,
		},
	}

	if identity.Address != nil && *identity.Address != "" {
		response.Address = identity.Address
	}

	if identity.Balance != nil {
		response.Balance = common.ToPointer(identity.Balance.String())
	}

	return response, nil
}

// RegisterStatic add method to the mux that are not documented in the API.
func RegisterStatic(mux *chi.Mux) {
	mux.Get("/", documentation)
	mux.Get("/static/docs/api/api.yaml", swagger)
	mux.Get("/favicon.ico", favicon)
}

func toVerifiableRefreshService(s *RefreshService) *verifiable.RefreshService {
	if s == nil {
		return nil
	}
	return &verifiable.RefreshService{
		ID:   s.Id,
		Type: verifiable.RefreshServiceType(s.Type),
	}
}

func toGetClaims200Response(claims []*verifiable.W3CCredential) GetClaims200JSONResponse {
	response := make(GetClaims200JSONResponse, len(claims))
	for i := range claims {
		response[i] = toGetClaim200Response(claims[i])
	}

	return response
}

func toGetClaim200Response(claim *verifiable.W3CCredential) GetClaimResponse {
	var claimExpiration, claimIssuanceDate *TimeUTC
	if claim.Expiration != nil {
		claimExpiration = common.ToPointer(TimeUTC(*claim.Expiration))
	}
	if claim.IssuanceDate != nil {
		claimIssuanceDate = common.ToPointer(TimeUTC(*claim.IssuanceDate))
	}

	var refreshService *RefreshService
	if claim.RefreshService != nil {
		refreshService = &RefreshService{
			Id:   claim.RefreshService.ID,
			Type: RefreshServiceType(claim.RefreshService.Type),
		}
	}

	return GetClaimResponse{
		Context: claim.Context,
		CredentialSchema: CredentialSchema{
			claim.CredentialSchema.ID,
			claim.CredentialSchema.Type,
		},
		CredentialStatus:  claim.CredentialStatus,
		CredentialSubject: claim.CredentialSubject,
		ExpirationDate:    claimExpiration,
		Id:                claim.ID,
		IssuanceDate:      claimIssuanceDate,
		Issuer:            claim.Issuer,
		Proof:             claim.Proof,
		Type:              claim.Type,
		RefreshService:    refreshService,
	}
}

func toGetClaimQrCode200JSONResponse(claim *domain.Claim, hostURL string) *GetClaimQrCode200JSONResponse {
	id := uuid.New()
	return &GetClaimQrCode200JSONResponse{
		Body: struct {
			Credentials []struct {
				Description string `json:"description"`
				Id          string `json:"id"`
			} `json:"credentials"`
			Url string `json:"url"`
		}{
			Credentials: []struct {
				Description string `json:"description"`
				Id          string `json:"id"`
			}{
				{
					Description: claim.SchemaType,
					Id:          claim.ID.String(),
				},
			},
			Url: fmt.Sprintf("%s/v1/agent", strings.TrimSuffix(hostURL, "/")),
		},
		From: claim.Issuer,
		Id:   id.String(),
		Thid: id.String(),
		To:   claim.OtherIdentifier,
		Typ:  string(packers.MediaTypePlainMessage),
		Type: string(protocol.CredentialOfferMessageType),
	}
}

func documentation(w http.ResponseWriter, _ *http.Request) {
	writeFile("api/spec.html", "text/html; charset=UTF-8", w)
}

func favicon(w http.ResponseWriter, _ *http.Request) {
	writeFile("api/polygon.png", "image/png", w)
}

func swagger(w http.ResponseWriter, _ *http.Request) {
	writeFile("api/api.yaml", "text/html; charset=UTF-8", w)
}

func writeFile(path string, mimeType string, w http.ResponseWriter) {
	f, err := os.ReadFile(path)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}
	w.Header().Set("Content-Type", mimeType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(f)
}
