/*
Copyright 2015 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package web

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/tls"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/user"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/net/websocket"
	"golang.org/x/text/encoding/unicode"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/auth/mocku2f"
	"github.com/gravitational/teleport/lib/backend"
	"github.com/gravitational/teleport/lib/client"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/fixtures"
	"github.com/gravitational/teleport/lib/httplib"
	"github.com/gravitational/teleport/lib/httplib/csrf"
	"github.com/gravitational/teleport/lib/pam"
	"github.com/gravitational/teleport/lib/reversetunnel"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/srv/regular"
	"github.com/gravitational/teleport/lib/sshutils"
	"github.com/gravitational/teleport/lib/state"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/teleport/lib/web/ui"
	"github.com/sirupsen/logrus"

	"github.com/gravitational/roundtrip"
	"github.com/gravitational/trace"

	"github.com/beevik/etree"
	"github.com/gokyle/hotp"
	"github.com/golang/protobuf/proto"
	"github.com/jonboulle/clockwork"
	"github.com/pquerna/otp/totp"
	"github.com/tstranex/u2f"
	. "gopkg.in/check.v1"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const hostID = "00000000-0000-0000-0000-000000000000"

func TestWeb(t *testing.T) {
	TestingT(t)
}

type WebSuite struct {
	node  *regular.Server
	proxy *regular.Server
	srvID string

	user      string
	webServer *httptest.Server
	freePorts []string

	mockU2F     *mocku2f.Key
	server      *auth.TestTLSServer
	proxyClient *auth.Client
}

var _ = Suite(&WebSuite{})

func (s *WebSuite) SetUpSuite(c *C) {
	var err error
	os.Unsetenv(teleport.DebugEnvVar)
	utils.InitLoggerForTests()

	// configure tests to use static assets from web/dist:
	debugAssetsPath = "../../web/dist"
	os.Setenv(teleport.DebugEnvVar, "true")

	//sessionStreamPollPeriod = time.Millisecond
	s.mockU2F, err = mocku2f.Create()
	c.Assert(err, IsNil)
	c.Assert(s.mockU2F, NotNil)
}

func (s *WebSuite) TearDownSuite(c *C) {
	os.Unsetenv(teleport.DebugEnvVar)
}

func (s *WebSuite) SetUpTest(c *C) {
	u, err := user.Current()
	c.Assert(err, IsNil)
	s.user = u.Username

	s.freePorts, err = utils.GetFreeTCPPorts(6)
	c.Assert(err, IsNil)

	authServer, err := auth.NewTestAuthServer(auth.TestAuthServerConfig{
		ClusterName: "localhost",
		Dir:         c.MkDir(),
	})
	c.Assert(err, IsNil)
	s.server, err = authServer.NewTestTLSServer()
	c.Assert(err, IsNil)

	// start node
	nodePort := s.freePorts[len(s.freePorts)-1]
	s.freePorts = s.freePorts[:len(s.freePorts)-1]

	certs, err := s.server.Auth().GenerateServerKeys(auth.GenerateServerKeysRequest{
		HostID:   hostID,
		NodeName: s.server.ClusterName(),
		Roles:    teleport.Roles{teleport.RoleNode},
	})
	c.Assert(err, IsNil)

	signer, err := sshutils.NewSigner(certs.Key, certs.Cert)
	c.Assert(err, IsNil)

	nodeClient, err := s.server.NewClient(auth.TestBuiltin(teleport.RoleNode))
	c.Assert(err, IsNil)

	// create SSH service:
	nodeDataDir := c.MkDir()
	node, err := regular.New(
		utils.NetAddr{AddrNetwork: "tcp", Addr: fmt.Sprintf("127.0.0.1:%v", nodePort)},
		s.server.ClusterName(),
		[]ssh.Signer{signer},
		nodeClient,
		nodeDataDir,
		"",
		utils.NetAddr{},
		regular.SetNamespace(defaults.Namespace),
		regular.SetShell("/bin/sh"),
		regular.SetSessionServer(nodeClient),
		regular.SetAuditLog(nodeClient),
		regular.SetPAMConfig(&pam.Config{Enabled: false}),
	)
	c.Assert(err, IsNil)
	s.node = node
	s.srvID = node.ID()
	c.Assert(s.node.Start(), IsNil)

	c.Assert(auth.CreateUploaderDir(nodeDataDir), IsNil)

	// create reverse tunnel service:
	s.proxyClient, err = s.server.NewClient(auth.TestBuiltin(teleport.RoleProxy))
	c.Assert(err, IsNil)

	revTunListener, err := net.Listen("tcp", fmt.Sprintf("%v:0", s.server.ClusterName()))
	c.Assert(err, IsNil)

	revTunServer, err := reversetunnel.NewServer(reversetunnel.Config{
		ID:                    node.ID(),
		Listener:              revTunListener,
		ClientTLS:             s.proxyClient.TLSConfig(),
		ClusterName:           s.server.ClusterName(),
		HostSigners:           []ssh.Signer{signer},
		LocalAuthClient:       s.proxyClient,
		LocalAccessPoint:      s.proxyClient,
		NewCachingAccessPoint: state.NoCache,
		DirectClusters:        []reversetunnel.DirectCluster{{Name: s.server.ClusterName(), Client: s.proxyClient}},
		DataDir:               c.MkDir(),
	})
	c.Assert(err, IsNil)

	// proxy server:
	proxyPort := s.freePorts[len(s.freePorts)-1]
	s.freePorts = s.freePorts[:len(s.freePorts)-1]
	proxyAddr := utils.NetAddr{
		AddrNetwork: "tcp", Addr: fmt.Sprintf("127.0.0.1:%v", proxyPort),
	}
	s.proxy, err = regular.New(proxyAddr,
		s.server.ClusterName(),
		[]ssh.Signer{signer},
		s.proxyClient,
		c.MkDir(),
		"",
		utils.NetAddr{},
		regular.SetProxyMode(revTunServer),
		regular.SetSessionServer(s.proxyClient),
		regular.SetAuditLog(s.proxyClient),
		regular.SetNamespace(defaults.Namespace),
	)
	c.Assert(err, IsNil)

	handler, err := NewHandler(Config{
		Proxy:        revTunServer,
		AuthServers:  utils.FromAddr(s.server.Addr()),
		DomainName:   s.server.ClusterName(),
		ProxyClient:  s.proxyClient,
		CipherSuites: utils.DefaultCipherSuites(),
	}, SetSessionStreamPollPeriod(200*time.Millisecond))
	c.Assert(err, IsNil)

	s.webServer = httptest.NewUnstartedServer(handler)
	s.webServer.StartTLS()
	err = s.proxy.Start()
	c.Assert(err, IsNil)

	addr, _ := utils.ParseAddr(s.webServer.Listener.Addr().String())
	handler.handler.cfg.ProxyWebAddr = *addr
	handler.handler.cfg.ProxySSHAddr = proxyAddr
}

func (s *WebSuite) TearDownTest(c *C) {
	c.Assert(s.node.Close(), IsNil)
	c.Assert(s.server.Close(), IsNil)
	s.webServer.Close()
	s.proxy.Close()
}

type authPack struct {
	otpSecret string
	user      string
	login     string
	otp       *hotp.HOTP
	session   *CreateSessionResponse
	clt       *client.WebClient
	cookies   []*http.Cookie
}

func (s *WebSuite) authPackFromResponse(c *C, re *roundtrip.Response) *authPack {
	var sess *createSessionResponseRaw
	c.Assert(json.Unmarshal(re.Bytes(), &sess), IsNil)

	jar, err := cookiejar.New(nil)
	c.Assert(err, IsNil)

	clt := s.client(roundtrip.BearerAuth(sess.Token), roundtrip.CookieJar(jar))
	jar.SetCookies(s.url(), re.Cookies())

	session, err := sess.response()
	if err != nil {
		panic(err)
	}
	if session.ExpiresIn < 0 {
		c.Errorf("expected expiry time to be in the future but got %v", session.ExpiresIn)
	}
	return &authPack{
		session: session,
		clt:     clt,
		cookies: re.Cookies(),
	}
}

// authPack returns new authenticated package consisting of created valid
// user, otp token, created web session and authenticated client.
func (s *WebSuite) authPack(c *C, user string) *authPack {
	login := s.user
	pass := "abc123"
	rawSecret := "def456"
	otpSecret := base32.StdEncoding.EncodeToString([]byte(rawSecret))

	ap, err := services.NewAuthPreference(services.AuthPreferenceSpecV2{
		Type:         teleport.Local,
		SecondFactor: teleport.OTP,
	})
	c.Assert(err, IsNil)
	err = s.server.Auth().SetAuthPreference(ap)
	c.Assert(err, IsNil)

	s.createUser(c, user, login, pass, otpSecret)

	// create a valid otp token
	validToken, err := totp.GenerateCode(otpSecret, time.Now())
	c.Assert(err, IsNil)

	clt := s.client()
	req := createSessionReq{
		User:              user,
		Pass:              pass,
		SecondFactorToken: validToken,
	}

	csrfToken := "2ebcb768d0090ea4368e42880c970b61865c326172a4a2343b645cf5d7f20992"
	re, err := s.login(clt, csrfToken, csrfToken, req)
	c.Assert(err, IsNil)

	var rawSess *createSessionResponseRaw
	c.Assert(json.Unmarshal(re.Bytes(), &rawSess), IsNil)

	sess, err := rawSess.response()
	c.Assert(err, IsNil)

	jar, err := cookiejar.New(nil)
	c.Assert(err, IsNil)

	clt = s.client(roundtrip.BearerAuth(sess.Token), roundtrip.CookieJar(jar))
	jar.SetCookies(s.url(), re.Cookies())

	return &authPack{
		otpSecret: otpSecret,
		user:      user,
		login:     login,
		session:   sess,
		clt:       clt,
		cookies:   re.Cookies(),
	}
}

func (s *WebSuite) createUser(c *C, user string, login string, pass string, otpSecret string) {
	teleUser, err := services.NewUser(user)
	c.Assert(err, IsNil)
	role := services.RoleForUser(teleUser)
	role.SetLogins(services.Allow, []string{login})
	options := role.GetOptions()
	options.ForwardAgent = services.NewBool(true)
	role.SetOptions(options)
	err = s.server.Auth().UpsertRole(role, backend.Forever)
	c.Assert(err, IsNil)
	teleUser.AddRole(role.GetName())

	err = s.server.Auth().UpsertUser(teleUser)
	c.Assert(err, IsNil)

	err = s.server.Auth().UpsertPassword(user, []byte(pass))
	c.Assert(err, IsNil)

	err = s.server.Auth().UpsertTOTP(user, otpSecret)
	c.Assert(err, IsNil)
}

func (s *WebSuite) TestNewUser(c *C) {
	token, err := s.server.Auth().CreateSignupToken(services.UserV1{Name: "bob", AllowedLogins: []string{s.user}}, 0)
	c.Assert(err, IsNil)

	// Save the original signup token, after GET /v2/webapi/users/invites/<token>
	// this should change.
	ost, err := s.server.Auth().GetSignupToken(token)
	c.Assert(err, IsNil)

	tokens, err := s.server.Auth().GetTokens()
	c.Assert(err, IsNil)
	c.Assert(len(tokens), Equals, 1)
	c.Assert(tokens[0].Token, Equals, token)

	clt := s.client()
	re, err := clt.Get(context.Background(), clt.Endpoint("webapi", "users", "invites", token), url.Values{})
	c.Assert(err, IsNil)

	var out *renderUserInviteResponse
	c.Assert(json.Unmarshal(re.Bytes(), &out), IsNil)
	c.Assert(out.User, Equals, "bob")
	c.Assert(out.InviteToken, Equals, token)

	st, err := s.server.Auth().GetSignupToken(token)
	c.Assert(err, IsNil)

	// Make sure that the signup token changed after rending the endpoint
	// GET /v2/webapi/users/invites/<token> above.
	c.Assert(st, Not(Equals), ost)

	validToken, err := totp.GenerateCode(st.OTPKey, time.Now())
	c.Assert(err, IsNil)

	tempPass := "abc123"

	re, err = clt.PostJSON(context.Background(), clt.Endpoint("webapi", "users"), createNewUserReq{
		InviteToken:       token,
		Pass:              tempPass,
		SecondFactorToken: validToken,
	})
	c.Assert(err, IsNil)

	var rawSess *createSessionResponseRaw
	c.Assert(json.Unmarshal(re.Bytes(), &rawSess), IsNil)
	cookies := re.Cookies()
	c.Assert(len(cookies), Equals, 1)

	// now make sure we are logged in by calling authenticated method
	// we need to supply both session cookie and bearer token for
	// request to succeed
	jar, err := cookiejar.New(nil)
	c.Assert(err, IsNil)

	clt = s.client(roundtrip.BearerAuth(rawSess.Token), roundtrip.CookieJar(jar))
	jar.SetCookies(s.url(), re.Cookies())

	re, err = clt.Get(context.Background(), clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, IsNil)

	var sites *getSitesResponse
	c.Assert(json.Unmarshal(re.Bytes(), &sites), IsNil)

	// in absence of session cookie or bearer auth the same request fill fail

	// no session cookie:
	clt = s.client(roundtrip.BearerAuth(rawSess.Token))
	re, err = clt.Get(context.Background(), clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, NotNil)
	c.Assert(trace.IsAccessDenied(err), Equals, true)

	// no bearer token:
	clt = s.client(roundtrip.CookieJar(jar))
	re, err = clt.Get(context.Background(), clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, NotNil)
	c.Assert(trace.IsAccessDenied(err), Equals, true)
}

func (s *WebSuite) TestSAMLSuccess(c *C) {
	input := fixtures.SAMLOktaConnectorV2

	decoder := kyaml.NewYAMLOrJSONDecoder(strings.NewReader(input), 32*1024)
	var raw services.UnknownResource
	err := decoder.Decode(&raw)
	c.Assert(err, IsNil)

	connector, err := services.GetSAMLConnectorMarshaler().UnmarshalSAMLConnector(raw.Raw)
	c.Assert(err, IsNil)
	err = connector.CheckAndSetDefaults()

	role, err := services.NewRole(connector.GetAttributesToRoles()[0].Roles[0], services.RoleSpecV3{
		Options: services.RoleOptions{
			MaxSessionTTL: services.NewDuration(defaults.MaxCertDuration),
		},
		Allow: services.RoleConditions{
			NodeLabels: services.Labels{services.Wildcard: []string{services.Wildcard}},
			Namespaces: []string{defaults.Namespace},
			Rules: []services.Rule{
				services.NewRule(services.Wildcard, services.RW()),
			},
		},
	})
	c.Assert(err, IsNil)
	role.SetLogins(services.Allow, []string{s.user})
	err = s.server.Auth().UpsertRole(role, backend.Forever)
	c.Assert(err, IsNil)

	err = s.server.Auth().CreateSAMLConnector(connector)
	c.Assert(err, IsNil)
	s.server.AuthServer.AuthServer.SetClock(clockwork.NewFakeClockAt(time.Date(2017, 05, 10, 18, 53, 0, 0, time.UTC)))
	clt := s.clientNoRedirects()

	csrfToken := "2ebcb768d0090ea4368e42880c970b61865c326172a4a2343b645cf5d7f20992"

	baseURL, err := url.Parse(clt.Endpoint("webapi", "saml", "sso") + `?redirect_url=http://localhost/after;connector_id=` + connector.GetName())
	c.Assert(err, IsNil)
	req, err := http.NewRequest("GET", baseURL.String(), nil)
	addCSRFCookieToReq(req, csrfToken)
	re, err := clt.Client.RoundTrip(func() (*http.Response, error) {
		return clt.Client.HTTPClient().Do(req)
	})

	// we got a redirect
	locationURL := re.Headers().Get("Location")
	u, err := url.Parse(locationURL)
	c.Assert(err, IsNil)
	c.Assert(u.Scheme+"://"+u.Host+u.Path, Equals, fixtures.SAMLOktaSSO)
	data, err := base64.StdEncoding.DecodeString(u.Query().Get("SAMLRequest"))
	c.Assert(err, IsNil)
	buf, err := ioutil.ReadAll(flate.NewReader(bytes.NewReader(data)))
	c.Assert(err, IsNil)
	doc := etree.NewDocument()
	err = doc.ReadFromBytes(buf)
	c.Assert(err, IsNil)
	id := doc.Root().SelectAttr("ID")
	c.Assert(id, NotNil)

	authRequest, err := s.server.Auth().GetSAMLAuthRequest(id.Value)
	c.Assert(err, IsNil)

	// now swap the request id to the hardcoded one in fixtures
	authRequest.ID = fixtures.SAMLOktaAuthRequestID
	authRequest.CSRFToken = csrfToken
	s.server.Auth().Identity.CreateSAMLAuthRequest(*authRequest, backend.Forever)

	// now respond with pre-recorded request to the POST url
	in := &bytes.Buffer{}
	fw, err := flate.NewWriter(in, flate.DefaultCompression)
	c.Assert(err, IsNil)

	_, err = fw.Write([]byte(fixtures.SAMLOktaAuthnResponseXML))
	c.Assert(err, IsNil)
	err = fw.Close()
	c.Assert(err, IsNil)
	encodedResponse := base64.StdEncoding.EncodeToString(in.Bytes())
	c.Assert(encodedResponse, NotNil)

	// now send the response to the server to exchange it for auth session
	form := url.Values{}
	form.Add("SAMLResponse", encodedResponse)
	req, err = http.NewRequest("POST", clt.Endpoint("webapi", "saml", "acs"), strings.NewReader(form.Encode()))
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	addCSRFCookieToReq(req, csrfToken)
	c.Assert(err, IsNil)
	authRe, err := clt.Client.RoundTrip(func() (*http.Response, error) {
		return clt.Client.HTTPClient().Do(req)
	})

	c.Assert(err, IsNil)
	comment := Commentf("Response: %v", string(authRe.Bytes()))
	c.Assert(authRe.Code(), Equals, http.StatusFound, comment)
	// we have got valid session
	c.Assert(authRe.Headers().Get("Set-Cookie"), Not(Equals), "")
	// we are being redirected to orignal URL
	c.Assert(authRe.Headers().Get("Location"), Equals, "/after")
}

func (s *WebSuite) TestWebSessionsCRUD(c *C) {
	pack := s.authPack(c, "foo")

	// make sure we can use client to make authenticated requests
	re, err := pack.clt.Get(context.Background(), pack.clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, IsNil)

	var sites *getSitesResponse
	c.Assert(json.Unmarshal(re.Bytes(), &sites), IsNil)

	// now delete session
	_, err = pack.clt.Delete(
		context.Background(),
		pack.clt.Endpoint("webapi", "sessions"))
	c.Assert(err, IsNil)

	// subsequent requests trying to use this session will fail
	re, err = pack.clt.Get(context.Background(), pack.clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, NotNil)
	c.Assert(trace.IsAccessDenied(err), Equals, true)
}

func (s *WebSuite) TestNamespace(c *C) {
	pack := s.authPack(c, "foo")

	_, err := pack.clt.Get(context.Background(), pack.clt.Endpoint("webapi", "sites", s.server.ClusterName(), "namespaces", "..%252fevents%3f", "nodes"), url.Values{})
	c.Assert(err, NotNil)

	_, err = pack.clt.Get(context.Background(), pack.clt.Endpoint("webapi", "sites", s.server.ClusterName(), "namespaces", "default", "nodes"), url.Values{})
	c.Assert(err, IsNil)
}

func (s *WebSuite) TestCSRF(c *C) {
	type input struct {
		reqToken    string
		cookieToken string
	}

	// create a valid user
	user := "csrfuser"
	pass := "abc123"
	otpSecret := base32.StdEncoding.EncodeToString([]byte("def456"))
	s.createUser(c, user, user, pass, otpSecret)

	// create a valid login form request
	validToken, err := totp.GenerateCode(otpSecret, time.Now())
	c.Assert(err, IsNil)
	loginForm := createSessionReq{
		User:              user,
		Pass:              pass,
		SecondFactorToken: validToken,
	}

	encodedToken1 := "2ebcb768d0090ea4368e42880c970b61865c326172a4a2343b645cf5d7f20992"
	encodedToken2 := "bf355921bbf3ef3672a03e410d4194077dfa5fe863c652521763b3e7f81e7b11"
	invalid := []input{
		{reqToken: encodedToken2, cookieToken: encodedToken1},
		{reqToken: "", cookieToken: encodedToken1},
		{reqToken: "", cookieToken: ""},
		{reqToken: encodedToken1, cookieToken: ""},
	}

	clt := s.client()

	// valid
	_, err = s.login(clt, encodedToken1, encodedToken1, loginForm)
	c.Assert(err, IsNil)

	// invalid
	for i := range invalid {
		_, err := s.login(clt, invalid[i].cookieToken, invalid[i].reqToken, loginForm)
		c.Assert(err, NotNil)
		c.Assert(trace.IsAccessDenied(err), Equals, true)
	}
}

func (s *WebSuite) TestPasswordChange(c *C) {
	pack := s.authPack(c, "foo")
	fakeClock := clockwork.NewFakeClock()
	s.server.AuthServer.AuthServer.SetClock(fakeClock)

	validToken, err := totp.GenerateCode(pack.otpSecret, fakeClock.Now())
	c.Assert(err, IsNil)

	req := changePasswordReq{
		OldPassword:       []byte("abc123"),
		NewPassword:       []byte("abc1234"),
		SecondFactorToken: validToken,
	}

	_, err = pack.clt.PutJSON(context.Background(), pack.clt.Endpoint("webapi", "users", "password"), req)
	c.Assert(err, IsNil)
}

func (s *WebSuite) TestWebSessionsRenew(c *C) {
	pack := s.authPack(c, "foo")

	// make sure we can use client to make authenticated requests
	// before we issue this request, we will recover session id and bearer token
	//
	prevSessionCookie := *pack.cookies[0]
	prevBearerToken := pack.session.Token
	re, err := pack.clt.PostJSON(context.Background(), pack.clt.Endpoint("webapi", "sessions", "renew"), nil)
	c.Assert(err, IsNil)

	newPack := s.authPackFromResponse(c, re)

	// new session is functioning
	re, err = newPack.clt.Get(context.Background(), pack.clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, IsNil)

	// old session is stil valid too (until it expires)
	jar, err := cookiejar.New(nil)
	c.Assert(err, IsNil)
	oldClt := s.client(roundtrip.BearerAuth(prevBearerToken), roundtrip.CookieJar(jar))
	jar.SetCookies(s.url(), []*http.Cookie{&prevSessionCookie})
	re, err = oldClt.Get(context.Background(), pack.clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, IsNil)

	// now delete session
	_, err = newPack.clt.Delete(
		context.Background(),
		pack.clt.Endpoint("webapi", "sessions"))
	c.Assert(err, IsNil)

	// subsequent requests trying to use this session will fail
	re, err = newPack.clt.Get(context.Background(), pack.clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, NotNil)
	c.Assert(trace.IsAccessDenied(err), Equals, true)
}

func (s *WebSuite) TestWebSessionsBadInput(c *C) {
	user := "bob"
	pass := "abc123"
	rawSecret := "def456"
	otpSecret := base32.StdEncoding.EncodeToString([]byte(rawSecret))

	err := s.server.Auth().UpsertPassword(user, []byte(pass))
	c.Assert(err, IsNil)

	err = s.server.Auth().UpsertTOTP(user, otpSecret)
	c.Assert(err, IsNil)

	// create valid token
	validToken, err := totp.GenerateCode(otpSecret, time.Now())
	c.Assert(err, IsNil)

	clt := s.client()

	reqs := []createSessionReq{
		// empty request
		{},
		// missing user
		{
			Pass:              pass,
			SecondFactorToken: validToken,
		},
		// missing pass
		{
			User:              user,
			SecondFactorToken: validToken,
		},
		// bad pass
		{
			User:              user,
			Pass:              "bla bla",
			SecondFactorToken: validToken,
		},
		// bad hotp token
		{
			User:              user,
			Pass:              pass,
			SecondFactorToken: "bad token",
		},
		// missing hotp token
		{
			User: user,
			Pass: pass,
		},
	}
	for i, req := range reqs {
		_, err = clt.PostJSON(context.Background(), clt.Endpoint("webapi", "sessions"), req)
		c.Assert(err, NotNil, Commentf("tc %v", i))
		c.Assert(trace.IsAccessDenied(err), Equals, true, Commentf("tc %v %T is not access denied", i, err))
	}
}

type getSiteNodeResponse struct {
	Items []ui.Server `json:"items"`
}

func (s *WebSuite) TestGetSiteNodes(c *C) {
	pack := s.authPack(c, "foo")

	// get site nodes
	re, err := pack.clt.Get(context.Background(), pack.clt.Endpoint("webapi", "sites", s.server.ClusterName(), "nodes"), url.Values{})
	c.Assert(err, IsNil)

	nodes := getSiteNodeResponse{}
	c.Assert(json.Unmarshal(re.Bytes(), &nodes), IsNil)
	c.Assert(len(nodes.Items), Equals, 1)

	// get site nodes using shortcut
	re, err = pack.clt.Get(context.Background(), pack.clt.Endpoint("webapi", "sites", currentSiteShortcut, "nodes"), url.Values{})
	c.Assert(err, IsNil)

	nodes2 := getSiteNodeResponse{}
	c.Assert(json.Unmarshal(re.Bytes(), &nodes2), IsNil)
	c.Assert(len(nodes.Items), Equals, 1)
	c.Assert(nodes2, DeepEquals, nodes)
}

func (s *WebSuite) TestSiteNodeConnectInvalidSessionID(c *C) {
	_, err := s.makeTerminal(s.authPack(c, "foo"), session.ID("/../../../foo"))
	c.Assert(err, NotNil)
}

func (s *WebSuite) TestResolveServerHostPort(c *C) {
	sampleNode := services.ServerV2{}
	sampleNode.SetName("eca53e45-86a9-11e7-a893-0242ac0a0101")
	sampleNode.Spec.Hostname = "nodehostname"

	// valid cases
	validCases := []struct {
		server       string
		nodes        []services.Server
		expectedHost string
		expectedPort int
	}{
		{
			server:       "localhost",
			expectedHost: "localhost",
			expectedPort: 0,
		},
		{
			server:       "localhost:8080",
			expectedHost: "localhost",
			expectedPort: 8080,
		},
		{
			server:       "eca53e45-86a9-11e7-a893-0242ac0a0101",
			nodes:        []services.Server{&sampleNode},
			expectedHost: "nodehostname",
			expectedPort: 0,
		},
	}

	// invalid cases
	invalidCases := []struct {
		server      string
		expectedErr string
	}{
		{
			server:      ":22",
			expectedErr: "empty hostname",
		},
		{
			server:      ":",
			expectedErr: "empty hostname",
		},
		{
			server:      "",
			expectedErr: "empty server name",
		},
		{
			server:      "host:",
			expectedErr: "invalid port",
		},
		{
			server:      "host:port",
			expectedErr: "invalid port",
		},
	}

	for _, testCase := range validCases {
		host, port, err := resolveServerHostPort(testCase.server, testCase.nodes)
		c.Assert(err, IsNil, Commentf(testCase.server))
		c.Assert(host, Equals, testCase.expectedHost)
		c.Assert(port, Equals, testCase.expectedPort)
	}

	for _, testCase := range invalidCases {
		_, _, err := resolveServerHostPort(testCase.server, nil)
		c.Assert(err, NotNil, Commentf(testCase.expectedErr))
		c.Assert(err, ErrorMatches, ".*"+testCase.expectedErr+".*")
	}

}

func (s *WebSuite) TestNewTerminalHandler(c *C) {
	validNode := services.ServerV2{}
	validNode.SetName("eca53e45-86a9-11e7-a893-0242ac0a0101")
	validNode.Spec.Hostname = "nodehostname"

	validServer := "localhost"
	validLogin := "root"
	validSID := session.ID("eca53e45-86a9-11e7-a893-0242ac0a0101")
	validParams := session.TerminalParams{
		H: 1,
		W: 1,
	}

	makeProvider := func(server services.ServerV2) AuthProvider {
		return authProviderMock{
			server: server,
		}
	}

	// valid cases
	validCases := []struct {
		req          TerminalRequest
		authProvider AuthProvider
		expectedHost string
		expectedPort int
	}{
		{
			req: TerminalRequest{
				Login:     validLogin,
				Server:    validServer,
				SessionID: validSID,
				Term:      validParams,
			},
			authProvider: makeProvider(validNode),
			expectedHost: validServer,
			expectedPort: 0,
		},
		{
			req: TerminalRequest{
				Login:     validLogin,
				Server:    "eca53e45-86a9-11e7-a893-0242ac0a0101",
				SessionID: validSID,
				Term:      validParams,
			},
			authProvider: makeProvider(validNode),
			expectedHost: "nodehostname",
			expectedPort: 0,
		},
	}

	// invalid cases
	invalidCases := []struct {
		req          TerminalRequest
		authProvider AuthProvider
		expectedErr  string
	}{
		{
			expectedErr:  "invalid session",
			authProvider: makeProvider(validNode),
			req: TerminalRequest{
				SessionID: "",
				Login:     validLogin,
				Server:    validServer,
				Term:      validParams,
			},
		},
		{
			expectedErr:  "bad term dimensions",
			authProvider: makeProvider(validNode),
			req: TerminalRequest{
				SessionID: validSID,
				Login:     validLogin,
				Server:    validServer,
				Term: session.TerminalParams{
					H: -1,
					W: 0,
				},
			},
		},
		{
			expectedErr:  "invalid server name",
			authProvider: makeProvider(validNode),
			req: TerminalRequest{
				Server:    "localhost:port",
				SessionID: validSID,
				Login:     validLogin,
				Term:      validParams,
			},
		},
	}

	for _, testCase := range validCases {
		term, err := NewTerminal(testCase.req, testCase.authProvider, nil)
		c.Assert(err, IsNil)
		c.Assert(term.params, DeepEquals, testCase.req)
		c.Assert(term.hostName, Equals, testCase.expectedHost)
		c.Assert(term.hostPort, Equals, testCase.expectedPort)
	}

	for _, testCase := range invalidCases {
		_, err := NewTerminal(testCase.req, testCase.authProvider, nil)
		c.Assert(err, ErrorMatches, ".*"+testCase.expectedErr+".*")
	}
}

func (s *WebSuite) TestResizeTerminal(c *C) {
	sid := session.NewID()

	// Create a new user "foo", open a terminal to a new session, and wait for
	// it to be ready.
	pack1 := s.authPack(c, "foo")
	ws1, err := s.makeTerminal(pack1, sid)
	c.Assert(err, IsNil)
	defer ws1.Close()
	err = s.waitForRawEvent(ws1, 5*time.Second)
	c.Assert(err, IsNil)

	// Create a new user "bar", open a terminal to the session created above,
	// and wait for it to be ready.
	pack2 := s.authPack(c, "bar")
	ws2, err := s.makeTerminal(pack2, sid)
	c.Assert(err, IsNil)
	defer ws2.Close()
	err = s.waitForRawEvent(ws2, 5*time.Second)
	c.Assert(err, IsNil)

	// Look at the audit events for the first terminal. It should have two
	// resize events from the second terminal (80x25 default then 100x100). Only
	// the second terminal will get these because resize events are not sent
	// back to the originator.
	err = s.waitForResizeEvent(ws1, 5*time.Second)
	c.Assert(err, IsNil)
	err = s.waitForResizeEvent(ws1, 5*time.Second)
	c.Assert(err, IsNil)

	// Look at the stream events for the second terminal. We don't expect to see
	// any resize events yet. It will timeout.
	err = s.waitForResizeEvent(ws2, 1*time.Second)
	c.Assert(err, NotNil)

	// Resize the second terminal. This should be reflected on the first terminal
	// because resize events are not sent back to the originator.
	params, err := session.NewTerminalParamsFromInt(300, 120)
	c.Assert(err, IsNil)
	data, err := json.Marshal(events.EventFields{
		events.EventType:      events.ResizeEvent,
		events.EventNamespace: defaults.Namespace,
		events.SessionEventID: sid.String(),
		events.TerminalSize:   params.Serialize(),
	})
	envelope := &Envelope{
		Version: defaults.WebsocketVersion,
		Type:    defaults.WebsocketResize,
		Payload: string(data),
	}
	envelopeBytes, err := proto.Marshal(envelope)
	c.Assert(err, IsNil)
	websocket.Message.Send(ws2, envelopeBytes)

	// This time the first terminal will see the resize event.
	err = s.waitForResizeEvent(ws1, 5*time.Second)
	c.Assert(err, IsNil)

	// The second terminal will not see any resize event. It will timeout.
	err = s.waitForResizeEvent(ws2, 1*time.Second)
	c.Assert(err, NotNil)
}

func (s *WebSuite) TestTerminal(c *C) {
	ws, err := s.makeTerminal(s.authPack(c, "foo"))
	c.Assert(err, IsNil)
	defer ws.Close()

	termHandler := newTerminalHandler()
	stream, err := termHandler.asTerminalStream(ws)
	c.Assert(err, IsNil)

	_, err = io.WriteString(stream, "echo vinsong\r\n")
	c.Assert(err, IsNil)

	err = s.waitForOutput(stream, "vinsong")
	c.Assert(err, IsNil)
}

func (s *WebSuite) TestWebAgentForward(c *C) {
	ws, err := s.makeTerminal(s.authPack(c, "foo"))
	c.Assert(err, IsNil)
	defer ws.Close()

	termHandler := newTerminalHandler()
	stream, err := termHandler.asTerminalStream(ws)
	c.Assert(err, IsNil)

	_, err = io.WriteString(stream, "echo $SSH_AUTH_SOCK\r\n")
	c.Assert(err, IsNil)

	err = s.waitForOutput(stream, "/")
	c.Assert(err, IsNil)
}

func (s *WebSuite) TestActiveSessions(c *C) {
	sid := session.NewID()
	pack := s.authPack(c, "foo")

	ws, err := s.makeTerminal(pack, sid)
	c.Assert(err, IsNil)
	defer ws.Close()

	termHandler := newTerminalHandler()
	stream, err := termHandler.asTerminalStream(ws)
	c.Assert(err, IsNil)

	// To make sure we have a session.
	_, err = io.WriteString(stream, "echo vinsong\r\n")
	c.Assert(err, IsNil)

	// Make sure server has replied.
	err = s.waitForOutput(stream, "vinsong")
	c.Assert(err, IsNil)

	// Make sure this session appears in the list of active sessions.
	var sessResp *siteSessionsGetResponse
	for i := 0; i < 10; i++ {
		// Get site nodes and make sure the node has our active party.
		re, err := pack.clt.Get(context.Background(), pack.clt.Endpoint("webapi", "sites", s.server.ClusterName(), "sessions"), url.Values{})
		c.Assert(err, IsNil)

		c.Assert(json.Unmarshal(re.Bytes(), &sessResp), IsNil)
		c.Assert(len(sessResp.Sessions), Equals, 1)

		// Sessions do not appear momentarily as there's async heartbeat
		// procedure.
		time.Sleep(250 * time.Millisecond)
	}

	c.Assert(len(sessResp.Sessions), Equals, 1)
	c.Assert(sessResp.Sessions[0].ID, Equals, sid)
}

func (s *WebSuite) TestCloseConnectionsOnLogout(c *C) {
	sid := session.NewID()
	pack := s.authPack(c, "foo")

	ws, err := s.makeTerminal(pack, sid)
	c.Assert(err, IsNil)
	defer ws.Close()

	termHandler := newTerminalHandler()
	stream, err := termHandler.asTerminalStream(ws)
	c.Assert(err, IsNil)

	// to make sure we have a session
	_, err = io.WriteString(stream, "expr 137 + 39\r\n")
	c.Assert(err, IsNil)

	// make sure server has replied
	out := make([]byte, 100)
	stream.Read(out)

	_, err = pack.clt.Delete(
		context.Background(),
		pack.clt.Endpoint("webapi", "sessions"))
	c.Assert(err, IsNil)

	// wait until we timeout or detect that connection has been closed
	after := time.After(5 * time.Second)
	errC := make(chan error)
	go func() {
		for {
			_, err := stream.Read(out)
			if err != nil {
				errC <- err
			}
		}
	}()

	select {
	case <-after:
		c.Fatalf("timeout")
	case err := <-errC:
		c.Assert(err, Equals, io.EOF)
	}
}

func (s *WebSuite) TestCreateSession(c *C) {
	pack := s.authPack(c, "foo")

	sess := session.Session{
		TerminalParams: session.TerminalParams{W: 300, H: 120},
		Login:          s.user,
	}

	re, err := pack.clt.PostJSON(
		context.Background(),
		pack.clt.Endpoint("webapi", "sites", s.server.ClusterName(), "sessions"),
		siteSessionGenerateReq{Session: sess},
	)
	c.Assert(err, IsNil)

	var created *siteSessionGenerateResponse
	c.Assert(json.Unmarshal(re.Bytes(), &created), IsNil)
	c.Assert(created.Session.ID, Not(Equals), "")
}

func (s *WebSuite) TestPlayback(c *C) {
	pack := s.authPack(c, "foo")
	sid := session.NewID()
	ws, err := s.makeTerminal(pack, sid)
	c.Assert(err, IsNil)
	defer ws.Close()
}

func (s *WebSuite) TestNewU2FUser(c *C) {
	// configure cluster authentication preferences
	cap, err := services.NewAuthPreference(services.AuthPreferenceSpecV2{
		Type:         teleport.Local,
		SecondFactor: teleport.U2F,
		U2F: &services.U2F{
			AppID:  "https://" + s.server.ClusterName(),
			Facets: []string{"https://" + s.server.ClusterName()},
		},
	})
	c.Assert(err, IsNil)
	err = s.server.AuthServer.AuthServer.SetAuthPreference(cap)
	c.Assert(err, IsNil)

	token, err := s.server.Auth().CreateSignupToken(services.UserV1{Name: "bob", AllowedLogins: []string{s.user}}, 0)
	c.Assert(err, IsNil)

	clt := s.client()
	re, err := clt.Get(context.Background(), clt.Endpoint("webapi", "u2f", "signuptokens", token), url.Values{})
	c.Assert(err, IsNil)

	var u2fRegReq u2f.RegisterRequest
	c.Assert(json.Unmarshal(re.Bytes(), &u2fRegReq), IsNil)

	u2fRegResp, err := s.mockU2F.RegisterResponse(&u2fRegReq)
	c.Assert(err, IsNil)

	tempPass := "abc123"

	re, err = clt.PostJSON(context.Background(), clt.Endpoint("webapi", "u2f", "users"), createNewU2FUserReq{
		InviteToken:         token,
		Pass:                tempPass,
		U2FRegisterResponse: *u2fRegResp,
	})
	c.Assert(err, IsNil)

	var rawSess *createSessionResponseRaw
	c.Assert(json.Unmarshal(re.Bytes(), &rawSess), IsNil)
	cookies := re.Cookies()
	c.Assert(len(cookies), Equals, 1)

	// now make sure we are logged in by calling authenticated method
	// we need to supply both session cookie and bearer token for
	// request to succeed
	jar, err := cookiejar.New(nil)
	c.Assert(err, IsNil)

	clt = s.client(roundtrip.BearerAuth(rawSess.Token), roundtrip.CookieJar(jar))
	jar.SetCookies(s.url(), re.Cookies())

	re, err = clt.Get(context.Background(), clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, IsNil)

	var sites *getSitesResponse
	c.Assert(json.Unmarshal(re.Bytes(), &sites), IsNil)

	// in absence of session cookie or bearer auth the same request fill fail

	// no session cookie:
	clt = s.client(roundtrip.BearerAuth(rawSess.Token))
	re, err = clt.Get(context.Background(), clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, NotNil)
	c.Assert(trace.IsAccessDenied(err), Equals, true)

	// no bearer token:
	clt = s.client(roundtrip.CookieJar(jar))
	re, err = clt.Get(context.Background(), clt.Endpoint("webapi", "sites"), url.Values{})
	c.Assert(err, NotNil)
	c.Assert(trace.IsAccessDenied(err), Equals, true)
}

func (s *WebSuite) TestU2FLogin(c *C) {
	// configure cluster authentication preferences
	cap, err := services.NewAuthPreference(services.AuthPreferenceSpecV2{
		Type:         teleport.Local,
		SecondFactor: teleport.U2F,
		U2F: &services.U2F{
			AppID:  "https://" + s.server.ClusterName(),
			Facets: []string{"https://" + s.server.ClusterName()},
		},
	})
	c.Assert(err, IsNil)
	err = s.server.Auth().SetAuthPreference(cap)
	c.Assert(err, IsNil)

	token, err := s.server.Auth().CreateSignupToken(services.UserV1{Name: "bob", AllowedLogins: []string{s.user}}, 0)
	c.Assert(err, IsNil)

	u2fRegReq, err := s.proxyClient.GetSignupU2FRegisterRequest(token)
	c.Assert(err, IsNil)

	u2fRegResp, err := s.mockU2F.RegisterResponse(u2fRegReq)
	c.Assert(err, IsNil)

	tempPass := "abc123"

	_, err = s.proxyClient.CreateUserWithU2FToken(token, tempPass, *u2fRegResp)
	c.Assert(err, IsNil)

	// normal login
	clt := s.client()
	re, err := clt.PostJSON(context.Background(), clt.Endpoint("webapi", "u2f", "signrequest"), client.U2fSignRequestReq{
		User: "bob",
		Pass: tempPass,
	})
	c.Assert(err, IsNil)
	var u2fSignReq u2f.SignRequest
	c.Assert(json.Unmarshal(re.Bytes(), &u2fSignReq), IsNil)

	u2fSignResp, err := s.mockU2F.SignResponse(&u2fSignReq)
	c.Assert(err, IsNil)

	_, err = clt.PostJSON(context.Background(), clt.Endpoint("webapi", "u2f", "sessions"), u2fSignResponseReq{
		User:            "bob",
		U2FSignResponse: *u2fSignResp,
	})
	c.Assert(err, IsNil)

	// bad login: corrupted sign responses, should fail

	re, err = clt.PostJSON(context.Background(), clt.Endpoint("webapi", "u2f", "signrequest"), client.U2fSignRequestReq{
		User: "bob",
		Pass: tempPass,
	})
	c.Assert(err, IsNil)
	c.Assert(json.Unmarshal(re.Bytes(), &u2fSignReq), IsNil)

	u2fSignResp, err = s.mockU2F.SignResponse(&u2fSignReq)
	c.Assert(err, IsNil)

	// corrupted KeyHandle
	u2fSignRespCopy := u2fSignResp
	u2fSignRespCopy.KeyHandle = u2fSignRespCopy.KeyHandle + u2fSignRespCopy.KeyHandle

	_, err = clt.PostJSON(context.Background(), clt.Endpoint("webapi", "u2f", "sessions"), u2fSignResponseReq{
		User:            "bob",
		U2FSignResponse: *u2fSignRespCopy,
	})
	c.Assert(err, NotNil)

	// corrupted SignatureData
	u2fSignRespCopy = u2fSignResp
	u2fSignRespCopy.SignatureData = u2fSignRespCopy.SignatureData[:10] + u2fSignRespCopy.SignatureData[20:]

	_, err = clt.PostJSON(context.Background(), clt.Endpoint("webapi", "u2f", "sessions"), u2fSignResponseReq{
		User:            "bob",
		U2FSignResponse: *u2fSignRespCopy,
	})
	c.Assert(err, NotNil)

	// corrupted ClientData
	u2fSignRespCopy = u2fSignResp
	u2fSignRespCopy.ClientData = u2fSignRespCopy.ClientData[:10] + u2fSignRespCopy.ClientData[20:]

	_, err = clt.PostJSON(context.Background(), clt.Endpoint("webapi", "u2f", "sessions"), u2fSignResponseReq{
		User:            "bob",
		U2FSignResponse: *u2fSignRespCopy,
	})
	c.Assert(err, NotNil)

	// bad login: counter not increasing, should fail

	s.mockU2F.SetCounter(0)

	re, err = clt.PostJSON(context.Background(), clt.Endpoint("webapi", "u2f", "signrequest"), client.U2fSignRequestReq{
		User: "bob",
		Pass: tempPass,
	})
	c.Assert(err, IsNil)
	c.Assert(json.Unmarshal(re.Bytes(), &u2fSignReq), IsNil)

	u2fSignResp, err = s.mockU2F.SignResponse(&u2fSignReq)
	c.Assert(err, IsNil)

	_, err = clt.PostJSON(context.Background(), clt.Endpoint("webapi", "u2f", "sessions"), u2fSignResponseReq{
		User:            "bob",
		U2FSignResponse: *u2fSignResp,
	})
	c.Assert(err, NotNil)
}

// TestPing ensures that a response is returned by /webapi/ping
// and that that response body contains authentication information.
func (s *WebSuite) TestPing(c *C) {
	wc := s.client()

	re, err := wc.Get(context.Background(), wc.Endpoint("webapi", "ping"), url.Values{})
	c.Assert(err, IsNil)

	var out *client.PingResponse
	c.Assert(json.Unmarshal(re.Bytes(), &out), IsNil)

	preference, err := s.server.Auth().GetAuthPreference()
	c.Assert(err, IsNil)

	c.Assert(out.Auth.Type, Equals, preference.GetType())
	c.Assert(out.Auth.SecondFactor, Equals, preference.GetSecondFactor())
}

func (s *WebSuite) TestMultipleConnectors(c *C) {
	wc := s.client()

	// create two oidc connectors, one named "foo" and another named "bar"
	oidcConnectorSpec := services.OIDCConnectorSpecV2{
		RedirectURL:  "https://localhost:3080/v1/webapi/oidc/callback",
		ClientID:     "000000000000-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.example.com",
		ClientSecret: "AAAAAAAAAAAAAAAAAAAAAAAA",
		IssuerURL:    "https://oidc.example.com",
		Display:      "Login with Example",
		Scope:        []string{"group"},
		ClaimsToRoles: []services.ClaimMapping{
			{
				Claim: "group",
				Value: "admin",
				Roles: []string{"admin"},
			},
		},
	}
	err := s.server.Auth().UpsertOIDCConnector(services.NewOIDCConnector("foo", oidcConnectorSpec))
	c.Assert(err, IsNil)
	err = s.server.Auth().UpsertOIDCConnector(services.NewOIDCConnector("bar", oidcConnectorSpec))
	c.Assert(err, IsNil)

	// set the auth preferences to oidc with no connector name
	authPreference, err := services.NewAuthPreference(services.AuthPreferenceSpecV2{
		Type: "oidc",
	})
	c.Assert(err, IsNil)
	err = s.server.Auth().SetAuthPreference(authPreference)
	c.Assert(err, IsNil)

	// hit the ping endpoint to get the auth type and connector name
	re, err := wc.Get(context.Background(), wc.Endpoint("webapi", "ping"), url.Values{})
	c.Assert(err, IsNil)
	var out *client.PingResponse
	c.Assert(json.Unmarshal(re.Bytes(), &out), IsNil)

	// make sure the connector name we got back was the first connector
	// in the backend, in this case it's "bar"
	oidcConnectors, err := s.server.Auth().GetOIDCConnectors(false)
	c.Assert(err, IsNil)
	c.Assert(out.Auth.OIDC.Name, Equals, oidcConnectors[0].GetName())

	// update the auth preferences and this time specify the connector name
	authPreference, err = services.NewAuthPreference(services.AuthPreferenceSpecV2{
		Type:          "oidc",
		ConnectorName: "foo",
	})
	c.Assert(err, IsNil)
	err = s.server.Auth().SetAuthPreference(authPreference)
	c.Assert(err, IsNil)

	// hit the ping endpoing to get the auth type and connector name
	re, err = wc.Get(context.Background(), wc.Endpoint("webapi", "ping"), url.Values{})
	c.Assert(err, IsNil)
	c.Assert(json.Unmarshal(re.Bytes(), &out), IsNil)

	// make sure the connector we get back is "foo"
	c.Assert(out.Auth.OIDC.Name, Equals, "foo")
}

type authProviderMock struct {
	server services.ServerV2
}

func (mock authProviderMock) GetNodes(n string, opts ...services.MarshalOption) ([]services.Server, error) {
	return []services.Server{&mock.server}, nil
}

func (mock authProviderMock) GetSessionEvents(n string, s session.ID, c int, p bool) ([]events.EventFields, error) {
	return []events.EventFields{}, nil
}

func (s *WebSuite) makeTerminal(pack *authPack, opts ...session.ID) (*websocket.Conn, error) {
	var sessionID session.ID
	if len(opts) == 0 {
		sessionID = session.NewID()
	} else {
		sessionID = opts[0]
	}

	u := url.URL{
		Host:   s.url().Host,
		Scheme: client.WSS,
		Path:   fmt.Sprintf("/v1/webapi/sites/%v/connect", currentSiteShortcut),
	}
	data, err := json.Marshal(TerminalRequest{
		Server: s.srvID,
		Login:  pack.login,
		Term: session.TerminalParams{
			W: 100,
			H: 100,
		},
		SessionID: sessionID,
	})
	if err != nil {
		return nil, err
	}

	q := u.Query()
	q.Set("params", string(data))
	q.Set(roundtrip.AccessTokenQueryParam, pack.session.Token)
	u.RawQuery = q.Encode()

	wscfg, err := websocket.NewConfig(u.String(), "http://localhost")
	wscfg.TlsConfig = &tls.Config{
		InsecureSkipVerify: true,
	}
	if err != nil {
		return nil, err
	}

	for _, cookie := range pack.cookies {
		wscfg.Header.Add("Cookie", cookie.String())
	}

	ws, err := websocket.DialConfig(wscfg)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return ws, nil
}

func (s *WebSuite) waitForOutput(stream *terminalStream, substr string) error {
	tickerCh := time.Tick(250 * time.Millisecond)
	timeoutCh := time.After(10 * time.Second)

	for {
		select {
		case <-tickerCh:
			out := make([]byte, 100)
			_, err := stream.Read(out)
			if err != nil {
				return trace.Wrap(err)
			}
			if strings.Contains(removeSpace(string(out)), substr) {
				return nil
			}
		case <-timeoutCh:
			return trace.BadParameter("timeout waiting on terminal for output: %v", substr)
		}
	}
}

func (s *WebSuite) waitForRawEvent(ws *websocket.Conn, timeout time.Duration) error {
	timeoutContext, timeoutCancel := context.WithTimeout(context.Background(), timeout)
	defer timeoutCancel()
	doneContext, doneCancel := context.WithCancel(context.Background())
	defer doneCancel()

	go func() {
		for {
			time.Sleep(250 * time.Millisecond)

			var raw []byte
			err := websocket.Message.Receive(ws, &raw)
			if err != nil {
				continue
			}

			var envelope Envelope
			err = proto.Unmarshal(raw, &envelope)
			if err != nil {
				continue
			}

			if envelope.GetType() == defaults.WebsocketRaw {
				doneCancel()
				return
			}
		}
	}()

	for {
		select {
		case <-timeoutContext.Done():
			return trace.BadParameter("timeout waiting for resize event")
		case <-doneContext.Done():
			return nil
		}
	}
}

func (s *WebSuite) waitForResizeEvent(ws *websocket.Conn, timeout time.Duration) error {
	timeoutContext, timeoutCancel := context.WithTimeout(context.Background(), timeout)
	defer timeoutCancel()
	doneContext, doneCancel := context.WithCancel(context.Background())
	defer doneCancel()

	go func() {
		for {
			time.Sleep(250 * time.Millisecond)

			var raw []byte
			err := websocket.Message.Receive(ws, &raw)
			if err != nil {
				continue
			}

			var envelope Envelope
			err = proto.Unmarshal(raw, &envelope)
			if err != nil {
				continue
			}

			if envelope.GetType() != defaults.WebsocketAudit {
				continue
			}

			var e events.EventFields
			err = json.Unmarshal([]byte(envelope.GetPayload()), &e)
			if err != nil {
				continue
			}

			if e.GetType() == events.ResizeEvent {
				doneCancel()
				return
			}
		}
	}()

	for {
		select {
		case <-timeoutContext.Done():
			return trace.BadParameter("timeout waiting for resize event")
		case <-doneContext.Done():
			return nil
		}
	}
}

func (s *WebSuite) clientNoRedirects(opts ...roundtrip.ClientParam) *client.WebClient {
	hclient := client.NewInsecureWebClient()
	hclient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	opts = append(opts, roundtrip.HTTPClient(hclient))
	wc, err := client.NewWebClient(s.url().String(), opts...)
	if err != nil {
		panic(err)
	}
	return wc
}

func (s *WebSuite) client(opts ...roundtrip.ClientParam) *client.WebClient {
	opts = append(opts, roundtrip.HTTPClient(client.NewInsecureWebClient()))
	wc, err := client.NewWebClient(s.url().String(), opts...)
	if err != nil {
		panic(err)
	}
	return wc
}

func (s *WebSuite) login(clt *client.WebClient, cookieToken string, reqToken string, reqData interface{}) (*roundtrip.Response, error) {
	return httplib.ConvertResponse(clt.RoundTrip(func() (*http.Response, error) {
		data, err := json.Marshal(reqData)
		req, err := http.NewRequest("POST", clt.Endpoint("webapi", "sessions"), bytes.NewBuffer(data))
		if err != nil {
			return nil, err
		}
		addCSRFCookieToReq(req, cookieToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(csrf.HeaderName, reqToken)
		return clt.HTTPClient().Do(req)
	}))
}

func (s *WebSuite) url() *url.URL {
	u, err := url.Parse("https://" + s.webServer.Listener.Addr().String())
	if err != nil {
		panic(err)
	}
	return u
}

func addCSRFCookieToReq(req *http.Request, token string) {
	cookie := &http.Cookie{
		Name:  csrf.CookieName,
		Value: token,
	}

	req.AddCookie(cookie)
}

func removeSpace(in string) string {
	for _, c := range []string{"\n", "\r", "\t"} {
		in = strings.Replace(in, c, " ", -1)
	}
	return strings.TrimSpace(in)
}

func newTerminalHandler() TerminalHandler {
	return TerminalHandler{
		log:     logrus.WithFields(logrus.Fields{}),
		encoder: unicode.UTF8.NewEncoder(),
		decoder: unicode.UTF8.NewDecoder(),
	}
}
