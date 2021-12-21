package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"

	"github.com/atreya2011/kratos-test/generated/go/service"
	"github.com/go-openapi/strfmt"
	hydra "github.com/ory/hydra-client-go/client"
	hydra_admin "github.com/ory/hydra-client-go/client/admin"
	hydra_models "github.com/ory/hydra-client-go/models"
	kratos "github.com/ory/kratos-client-go"
	log "github.com/sirupsen/logrus"
)

var ctx = context.Background()

//go:embed templates
var templates embed.FS

// templateData contains data for template
type templateData struct {
	Title   string
	UI      *kratos.UiContainer
	Details string
}

// server contains server information
type server struct {
	KratosAPIClient      *kratos.APIClient
	KratosPublicEndpoint string
	HydraAPIClient       *hydra.OryHydra
	HydraPublicEndpoint  string
	Port                 string
}

func main() {
	// create server
	s, err := NewServer(4433, 4445)
	if err != nil {
		log.Fatalln(err)
	}

	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("/templates/static"))))

	http.HandleFunc("/login", s.handleLogin)
	http.HandleFunc("/logout", s.handleLogout)
	http.HandleFunc("/error", s.handleError)
	http.HandleFunc("/registration", s.ensureCookieFlowID("registration", s.handleRegister))
	http.HandleFunc("/verification", s.ensureCookieFlowID("verification", s.handleVerification))
	http.HandleFunc("/registered", ensureCookieReferer(s.handleRegistered))
	http.HandleFunc("/dashboard", s.handleDashboard)
	http.HandleFunc("/verified", ensureCookieReferer(s.handleVerified))
	http.HandleFunc("/recovery", s.ensureCookieFlowID("recovery", s.handleRecovery))
	http.HandleFunc("/settings", s.ensureCookieFlowID("settings", s.handleSettings))

	http.HandleFunc("/auth/consent", s.handleHydraConsent)

	// start server
	log.Println("Auth Server listening on port 8000")
	log.Fatalln(http.ListenAndServe(":8000", http.DefaultServeMux))
}

// handleLogin handles login request from hydra and kratos login flow
func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {

	log.Info("got request")

	// get login challenge from url query parameters
	challenge := r.URL.Query().Get("login_challenge")
	flowID := r.URL.Query().Get("flow")
	// redirect to login page if there is no login challenge or flow id in url query parameters
	if challenge == "" && flowID == "" {

		b := make([]byte, 32)
		_, err := rand.Read(b)
		if err != nil {
			log.Errorf("generate state failed: %v", err)
			return
		}

		state := base64.StdEncoding.EncodeToString(b)

		params := url.Values{
			"response_type": []string{"code"},
			"refresh_type":  []string{"code"},
			"client_id":     []string{"openaios-client"},
			"scope":         []string{"offline"},
			"redirect_uri":  []string{"http://kratos.dev.openaios.4pd.io/iam-web/dashboard"},
			"state":         []string{state},
		}
		redirectTo := fmt.Sprintf("%s/oauth2/auth?", s.HydraPublicEndpoint) + params.Encode()
		log.Infof("redirect to hydra, url: %s", redirectTo)
		http.Redirect(w, r, redirectTo, http.StatusFound)
		return
		//log.Println("No login challenge found or flow ID found in URL Query Parameters")
		//writeError(w, http.StatusUnauthorized, errors.New("Unauthorized OAuth Client"))
		//return
	}

	// get login request from hydra only if there is no flow id in the url query parameters
	if flowID == "" {
		//	s.HydraAPIClient
		_, err := s.HydraAPIClient.Admin.GetLoginRequest(&hydra_admin.GetLoginRequestParams{
			Context:        ctx,
			LoginChallenge: challenge,
		})
		if err != nil {
			log.Println(err)
			writeError(w, http.StatusUnauthorized, errors.New("Unauthorized OAuth Client"))
			return
		}
	}

	// get cookie from headers
	cookie := r.Header.Get("cookie")

	// check for kratos session details
	session, _, err := s.KratosAPIClient.V0alpha2Api.ToSession(ctx).Cookie(cookie).Execute()

	// if there is no session, redirect to login page with login challenge
	if err != nil {
		// build return_to url with hydra login challenge as url query parameter
		returnToParams := url.Values{
			"login_challenge": []string{challenge},
		}
		returnTo := "/iam-web/login?" + returnToParams.Encode()
		// build redirect url with return_to as url query parameter
		// refresh=true forces a new login from kratos regardless of browser sessions
		// this is important because we are letting Hydra handle sessions
		redirectToParam := url.Values{
			"return_to": []string{returnTo},
			"refresh":   []string{"true"},
		}
		redirectTo := fmt.Sprintf("%s/self-service/login/browser?", s.KratosPublicEndpoint) + redirectToParam.Encode()

		// get flowID from url query parameters
		flowID := r.URL.Query().Get("flow")

		// if there is no flow id in url query parameters, create a new flow
		if flowID == "" {
			http.Redirect(w, r, redirectTo, http.StatusFound)
			return
		}

		// get cookie from headers
		cookie := r.Header.Get("cookie")
		// get the login flow
		flow, _, err := s.KratosAPIClient.V0alpha2Api.GetSelfServiceLoginFlow(ctx).Id(flowID).Cookie(cookie).Execute()
		if err != nil {
			writeError(w, http.StatusUnauthorized, err)
			return
		}
		templateData := templateData{
			Title: "Login",
			UI:    &flow.Ui,
		}
		// render template index.html
		templateData.Render(w)
		return
	}

	// if there is a valid session, marshal session.identity.traits to json to be stored in subject
	traitsJSON, err := json.Marshal(session.Identity.Traits)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	subject := string(traitsJSON)

	// accept hydra login request
	res, err := s.HydraAPIClient.Admin.AcceptLoginRequest(&hydra_admin.AcceptLoginRequestParams{
		Context:        ctx,
		LoginChallenge: challenge,
		Body: &hydra_models.AcceptLoginRequest{
			Remember:    true,
			RememberFor: 3600,
			Subject:     &subject,
		},
	})
	if err != nil {
		log.Println(err)
		writeError(w, http.StatusUnauthorized, errors.New("Unauthorized OAuth Client"))
		return
	}
	http.Redirect(w, r, *res.GetPayload().RedirectTo, http.StatusFound)
}

// handleLogout handles kratos logout flow
func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// get cookie from headers
	cookie := r.Header.Get("cookie")
	// create self-service logout flow for browser
	flow, _, err := s.KratosAPIClient.V0alpha2Api.CreateSelfServiceLogoutFlowUrlForBrowsers(ctx).Cookie(cookie).Execute()
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	// redirect to logout url if session is valid
	if flow != nil {
		http.Redirect(w, r, flow.LogoutUrl, http.StatusFound)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// handleError handles login/registration error
func (s *server) handleError(w http.ResponseWriter, r *http.Request) {
	// get url query parameters
	errorID := r.URL.Query().Get("id")
	// get error details
	errorDetails, _, err := s.KratosAPIClient.V0alpha2Api.GetSelfServiceError(ctx).Id(errorID).Execute()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// marshal errorDetails to json
	errorDetailsJSON, err := json.MarshalIndent(errorDetails, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	templateData := templateData{
		Title:   "Error",
		Details: string(errorDetailsJSON),
	}
	// render template index.html
	templateData.Render(w)
}

// handleRegister handles kratos registration flow
func (s *server) handleRegister(w http.ResponseWriter, r *http.Request, cookie, flowID string) {
	// get the registration flow
	flow, _, err := s.KratosAPIClient.V0alpha2Api.GetSelfServiceRegistrationFlow(ctx).Id(flowID).Cookie(cookie).Execute()
	if err != nil {
		writeError(w, http.StatusUnauthorized, err)
		return
	}
	templateData := templateData{
		Title: "Registration",
		UI:    &flow.Ui,
	}
	// render template index.html
	templateData.Render(w)
}

// handleVerification handles kratos verification flow
func (s *server) handleVerification(w http.ResponseWriter, r *http.Request, cookie, flowID string) {
	// get self-service verification flow for browser
	flow, _, err := s.KratosAPIClient.V0alpha2Api.GetSelfServiceVerificationFlow(ctx).Id(flowID).Cookie(cookie).Execute()
	if err != nil {
		writeError(w, http.StatusUnauthorized, err)
		return
	}

	templateData := templateData{
		Title: "Verify your Email address",
		UI:    &flow.Ui,
	}
	// render template index.html
	templateData.Render(w)
}

// handleRegistered displays registration complete message to user
func (s *server) handleRegistered(w http.ResponseWriter, r *http.Request) {
	templateData := templateData{
		Title: "Registration Complete",
	}
	// render template index.html
	templateData.Render(w)
}

// handleVerified displays verfification complete message to user
func (s *server) handleVerified(w http.ResponseWriter, r *http.Request) {
	templateData := templateData{
		Title: "Verification Complete",
	}
	// render template index.html
	templateData.Render(w)
}

// handleRecovery handles kratos recovery flow
func (s *server) handleRecovery(w http.ResponseWriter, r *http.Request, cookie, flowID string) {
	// get self-service recovery flow for browser
	flow, _, err := s.KratosAPIClient.V0alpha2Api.GetSelfServiceRecoveryFlow(ctx).Id(flowID).Cookie(cookie).Execute()
	if err != nil {
		writeError(w, http.StatusUnauthorized, err)
		return
	}

	templateData := templateData{
		Title: "Password Recovery Form",
		UI:    &flow.Ui,
	}
	// render template index.html
	templateData.Render(w)
}

// handleSettings handles kratos settings flow
func (s *server) handleSettings(w http.ResponseWriter, r *http.Request, cookie, flowID string) {
	// get self-service recovery flow for browser
	flow, _, err := s.KratosAPIClient.V0alpha2Api.GetSelfServiceSettingsFlow(ctx).Id(flowID).Cookie(cookie).Execute()
	if err != nil {
		writeError(w, http.StatusUnauthorized, err)
		return
	}

	templateData := templateData{
		Title: "Settings",
		UI:    &flow.Ui,
	}
	// render template index.html
	templateData.Render(w)
}

// handleDashboard shows dashboard
func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	// get cookie from headers
	cookie := r.Header.Get("cookie")
	// get session details
	session, _, err := s.KratosAPIClient.V0alpha2Api.ToSession(ctx).Cookie(cookie).Execute()
	if err != nil {
		http.Redirect(w, r, "/iam-web/login", http.StatusFound)
		return
	}

	// marshal session to json
	sessionJSON, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	templateData := templateData{
		Title:   "Session Details",
		Details: string(sessionJSON),
	}
	// render template index.html
	templateData.Render(w)
}

// handleHydraConsent shows hydra consent screen
func (s *server) handleHydraConsent(w http.ResponseWriter, r *http.Request) {
	// get consent challenge from url query parameters
	challenge := r.URL.Query().Get("consent_challenge")

	if challenge == "" {
		log.Println("Missing consent challenge")
		writeError(w, http.StatusUnauthorized, errors.New("Unauthorized OAuth Client"))
		return
	}

	// get consent request
	getConsentRes, err := s.HydraAPIClient.Admin.GetConsentRequest(&hydra_admin.GetConsentRequestParams{
		Context:          ctx,
		ConsentChallenge: challenge,
	})
	if err != nil {
		log.Println(err)
		writeError(w, http.StatusUnauthorized, errors.New("Unauthorized OAuth Client"))
		return
	}

	// get cookie from headers
	cookie := r.Header.Get("cookie")
	// get session details
	session, _, err := s.KratosAPIClient.V0alpha2Api.ToSession(ctx).Cookie(cookie).Execute()
	if err != nil {
		log.Println(err)
		writeError(w, http.StatusUnauthorized, errors.New("Unauthorized OAuth Client"))
		return
	}

	log.Infof("handle hydra consent request, session:%v", session)
	log.Infof("handle hydra consent request, session.Identity:%v", session.Identity)

	var name string

	if trait, ok := session.Identity.Traits.(map[string]interface{}); ok {
		if s, ok := trait["username"].(string); ok {
			name = s
		}
	}

	// accept consent request and add verifiable address to id_token in session
	acceptConsentRes, err := s.HydraAPIClient.Admin.AcceptConsentRequest(&hydra_admin.AcceptConsentRequestParams{
		Context:          ctx,
		ConsentChallenge: challenge,
		Body: &hydra_models.AcceptConsentRequest{
			GrantScope:  getConsentRes.Payload.RequestedScope,
			Remember:    true,
			RememberFor: 3600,
			Session: &hydra_models.ConsentRequestSession{
				IDToken: service.PersonSchemaJsonTraits{Name: &service.PersonSchemaJsonTraitsName{First: &name}},
			},
		},
	})

	if err != nil {
		log.Errorf("accept consent request failure, err:%v", err)
		writeError(w, http.StatusUnauthorized, errors.New("Unauthorized OAuth Client"))
		return
	}

	log.Infof("accept consent request success, user:%s", name)

	log.Infof("accept consent request and redirect to dashboard")

	log.Infof("accept consent request,accept res:%v", acceptConsentRes)

	http.Redirect(w, r, "/iam-web/dashboard", http.StatusFound)
}

func NewServer(kratosPublicEndpointPort, hydraPublicEndpointPort int) (*server, error) {
	// create a new kratos client for self hosted server
	conf := kratos.NewConfiguration()
	conf.Servers = kratos.ServerConfigurations{{URL: "http://kratos.dev.openaios.4pd.io"}}
	cj, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	conf.HTTPClient = &http.Client{Jar: cj}

	return &server{
		KratosAPIClient:      kratos.NewAPIClient(conf),
		KratosPublicEndpoint: "http://kratos.dev.openaios.4pd.io",
		HydraAPIClient: hydra.NewHTTPClientWithConfig(strfmt.Default, &hydra.TransportConfig{
			BasePath: "/",
			Host:     "admin.hydra.dev.openaios.4pd.io",
			Schemes:  []string{"http"},
		}),
		HydraPublicEndpoint: "http://hydra.dev.openaios.4pd.io",
		Port:                ":80",
	}, nil
}

// writeError writes error to the response
func writeError(w http.ResponseWriter, statusCode int, err error) {
	w.WriteHeader(statusCode)
	if _, e := w.Write([]byte(err.Error())); e != nil {
		log.Fatal(err)
	}
}

// ensureCookieFlowID is a middleware function that ensures that a request contains
// flow ID in url query parameters and cookie in header
func (s *server) ensureCookieFlowID(flowType string, next func(w http.ResponseWriter, r *http.Request, cookie, flowID string)) http.HandlerFunc {
	// create redirect url based on flow type
	redirectURL := fmt.Sprintf("%s/self-service/%s/browser", s.KratosPublicEndpoint, flowType)

	return func(w http.ResponseWriter, r *http.Request) {
		// get flowID from url query parameters
		flowID := r.URL.Query().Get("flow")
		// if there is no flow id in url query parameters, create a new flow
		if flowID == "" {
			http.Redirect(w, r, redirectURL, http.StatusFound)
			return
		}

		// get cookie from headers
		cookie := r.Header.Get("cookie")
		// if there is no cookie in header, return error
		if cookie == "" {
			writeError(w, http.StatusBadRequest, errors.New("missing cookie"))
			return
		}

		// call next handler
		next(w, r, cookie, flowID)
	}
}

// ensureCookieReferer is a middleware function that ensures that cookie in header contains csrf_token and referer is not empty
func ensureCookieReferer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// get cookie from headers
		cookie := r.Header.Get("cookie")
		// if there is no csrf_token in cookie, return error
		if !strings.Contains(cookie, "csrf_token") {
			writeError(w, http.StatusUnauthorized, errors.New(http.StatusText(int(http.StatusUnauthorized))))
			return
		}

		// get referer from headers
		referer := r.Header.Get("referer")
		// if there is no referer in header, return error
		if referer == "" {
			writeError(w, http.StatusBadRequest, errors.New(http.StatusText(int(http.StatusUnauthorized))))
			return
		}

		// call next handler
		next(w, r)
	}
}

// Render renders template with provided data
func (td *templateData) Render(w http.ResponseWriter) {
	// render template index.html
	tmpl := template.Must(template.ParseFS(templates, "templates/index.html"))
	if err := tmpl.Execute(w, td); err != nil {
		writeError(w, http.StatusInternalServerError, err)
	}
}
