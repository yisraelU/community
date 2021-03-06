// Copyright 2016 Documize Inc. <legal@documize.com>. All rights reserved.
//
// This software (Documize Community Edition) is licensed under
// GNU AGPL v3 http://www.gnu.org/licenses/agpl-3.0.en.html
//
// You can operate outside the AGPL restrictions by purchasing
// Documize Enterprise Edition and obtaining a commercial license
// by contacting <sales@documize.com>.
//
// https://documize.com

package ldap

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"sort"
	"strings"

	"github.com/documize/community/core/env"
	"github.com/documize/community/core/response"
	"github.com/documize/community/core/secrets"
	"github.com/documize/community/core/streamutil"
	"github.com/documize/community/domain"
	"github.com/documize/community/domain/auth"
	usr "github.com/documize/community/domain/user"
	ath "github.com/documize/community/model/auth"
	lm "github.com/documize/community/model/auth"
	"github.com/documize/community/model/user"
)

// Handler contains the runtime information such as logging and database.
type Handler struct {
	Runtime *env.Runtime
	Store   *domain.Store
}

// Preview connects to LDAP using paylaod and returns first 50 users.
func (h *Handler) Preview(w http.ResponseWriter, r *http.Request) {
	h.Runtime.Log.Info("Sync'ing with LDAP")

	ctx := domain.GetRequestContext(r)
	if !ctx.Administrator {
		response.WriteForbiddenError(w)
		return
	}

	var result struct {
		Message string      `json:"message"`
		IsError bool        `json:"isError"`
		Users   []user.User `json:"users"`
		Count   int         `json:"count"`
	}

	result.IsError = true
	result.Users = []user.User{}

	// Read the request.
	defer streamutil.Close(r.Body)
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		result.Message = "Error: unable read request body"
		result.IsError = true
		response.WriteJSON(w, result)
		h.Runtime.Log.Error(result.Message, err)
		return
	}

	// Decode LDAP config.
	c := lm.LDAPConfig{}
	err = json.Unmarshal(body, &c)
	if err != nil {
		result.Message = "Error: unable read LDAP configuration payload"
		result.IsError = true
		response.WriteJSON(w, result)
		h.Runtime.Log.Error(result.Message, err)
		return
	}

	if c.ServerPort == 0 && len(c.ServerHost) == 0 {
		result.Message = "Missing LDAP server details"
		result.IsError = true
		response.WriteJSON(w, result)
		return
	}
	if len(c.BindDN) == 0 && len(c.BindPassword) == 0 {
		result.Message = "Missing LDAP bind credentials"
		result.IsError = true
		response.WriteJSON(w, result)
		return
	}
	if len(c.UserFilter) == 0 && len(c.GroupFilter) == 0 {
		result.Message = "Missing LDAP search filters"
		result.IsError = true
		response.WriteJSON(w, result)
		return
	}

	h.Runtime.Log.Info("Fetching LDAP users")

	users, err := fetchUsers(c)
	if err != nil {
		result.Message = "Error: unable fetch users from LDAP"
		result.IsError = true
		response.WriteJSON(w, result)
		h.Runtime.Log.Error(result.Message, err)
		return
	}

	result.IsError = false
	result.Message = fmt.Sprintf("Previewing LDAP, found %d users", len(users))
	result.Count = len(users)
	result.Users = users

	// Preview does not require more than 50 users.
	if len(users) > 50 {
		result.Users = users[:50]
	}

	h.Runtime.Log.Info(result.Message)

	response.WriteJSON(w, result)
}

// Sync gets list of Keycloak users and inserts new users into Documize
// and marks Keycloak disabled users as inactive.
func (h *Handler) Sync(w http.ResponseWriter, r *http.Request) {
	ctx := domain.GetRequestContext(r)

	if !ctx.Administrator {
		response.WriteForbiddenError(w)
		return
	}

	var result struct {
		Message string `json:"message"`
		IsError bool   `json:"isError"`
	}

	result.IsError = true
	result.Message = "Unable to connect to LDAP"

	// Org contains raw auth provider config
	org, err := h.Store.Organization.GetOrganization(ctx, ctx.OrgID)
	if err != nil {
		result.Message = "Error: unable to get organization record"
		result.IsError = true
		response.WriteJSON(w, result)
		h.Runtime.Log.Error(result.Message, err)
		return
	}

	// Exit if not using LDAP
	if org.AuthProvider != ath.AuthProviderLDAP {
		result.Message = "Error: skipping user sync with LDAP as it is not the configured option"
		result.IsError = true
		response.WriteJSON(w, result)
		h.Runtime.Log.Info(result.Message)
		return
	}

	// Get auth provider config
	c := lm.LDAPConfig{}
	err = json.Unmarshal([]byte(org.AuthConfig), &c)
	if err != nil {
		result.Message = "Error: unable read LDAP configuration data"
		result.IsError = true
		response.WriteJSON(w, result)
		h.Runtime.Log.Error(result.Message, err)
		return
	}

	// Get user list from LDAP.
	ldapUsers, err := fetchUsers(c)
	if err != nil {
		result.Message = "Error: unable to fetch LDAP users: " + err.Error()
		result.IsError = true
		response.WriteJSON(w, result)
		h.Runtime.Log.Error(result.Message, err)
		return
	}

	// Get user list from Documize
	dmzUsers, err := h.Store.User.GetUsersForOrganization(ctx, "", 99999)
	if err != nil {
		result.Message = "Error: unable to fetch Documize users"
		result.IsError = true
		response.WriteJSON(w, result)
		h.Runtime.Log.Error(result.Message, err)
		return
	}

	sort.Slice(ldapUsers, func(i, j int) bool { return ldapUsers[i].Email < ldapUsers[j].Email })
	sort.Slice(dmzUsers, func(i, j int) bool { return dmzUsers[i].Email < dmzUsers[j].Email })

	insert := []user.User{}

	for _, k := range ldapUsers {
		exists := false
		for _, d := range dmzUsers {
			if k.Email == d.Email {
				exists = true
			}
		}
		if !exists {
			insert = append(insert, k)
		}
	}

	// Track the number of LDAP users with missing data.
	missing := 0

	// Insert new users into Documize
	for _, u := range insert {
		if len(u.Email) == 0 {
			missing++
		} else {
			_, err = auth.AddExternalUser(ctx, h.Runtime, h.Store, u, c.DefaultPermissionAddSpace)
		}
	}

	result.IsError = false
	result.Message = "Sync complete with LDAP server"
	result.Message = fmt.Sprintf(
		"LDAP sync found %d users, %d new users added, %d users with missing data ignored",
		len(ldapUsers), len(insert), missing)

	h.Runtime.Log.Info(result.Message)

	response.WriteJSON(w, result)
}

// Authenticate checks LDAP authentication credentials.
func (h *Handler) Authenticate(w http.ResponseWriter, r *http.Request) {
	method := "ldap.authenticate"
	ctx := domain.GetRequestContext(r)

	// check for http header
	authHeader := r.Header.Get("Authorization")
	if len(authHeader) == 0 {
		response.WriteBadRequestError(w, method, "Missing Authorization header")
		return
	}

	// decode what we received
	data := strings.Replace(authHeader, "Basic ", "", 1)

	decodedBytes, err := secrets.DecodeBase64([]byte(data))
	if err != nil {
		response.WriteBadRequestError(w, method, "Unable to decode authentication token")
		h.Runtime.Log.Error("decode auth header", err)
		return
	}

	decoded := string(decodedBytes)

	// check that we have domain:username:password (but allow for : in password field!)
	credentials := strings.SplitN(decoded, ":", 3)
	if len(credentials) != 3 {
		response.WriteBadRequestError(w, method, "Bad authentication token, expecting domain:username:password")
		h.Runtime.Log.Error("bad auth token", err)
		return
	}

	dom := strings.TrimSpace(strings.ToLower(credentials[0]))
	username := strings.TrimSpace(strings.ToLower(credentials[1]))
	password := credentials[2]

	// Check for required fields.
	if len(username) == 0 || len(password) == 0 {
		response.WriteUnauthorizedError(w)
		return
	}

	dom = h.Store.Organization.CheckDomain(ctx, dom) // TODO optimize by removing this once js allows empty domains

	h.Runtime.Log.Info("LDAP login request " + username + " @ " + dom)

	// Get the org and it's associated LDAP config.
	org, err := h.Store.Organization.GetOrganizationByDomain(dom)
	if err != nil {
		response.WriteUnauthorizedError(w)
		h.Runtime.Log.Error("bad auth organization", err)
		return
	}

	lc := lm.LDAPConfig{}
	err = json.Unmarshal([]byte(org.AuthConfig), &lc)
	if err != nil {
		response.WriteBadRequestError(w, method, "unable to read LDAP config during authorization")
		h.Runtime.Log.Error(method, err)
		return
	}

	ctx.OrgID = org.RefID

	l, err := connect(lc)
	if err != nil {
		response.WriteBadRequestError(w, method, "unable to dial LDAP server")
		h.Runtime.Log.Error(method, err)
		return
	}
	defer l.Close()

	lu, ok, err := authenticate(l, lc, username, password)
	if err != nil {
		response.WriteBadRequestError(w, method, "error during LDAP authentication")
		h.Runtime.Log.Error(method, err)
		return
	}
	if !ok {
		response.WriteUnauthorizedError(w)
		return
	}

	h.Runtime.Log.Info("LDAP logon completed " + lu.Email)

	u, err := h.Store.User.GetByDomain(ctx, dom, lu.Email)
	if err != nil && err != sql.ErrNoRows {
		response.WriteServerError(w, method, err)
		h.Runtime.Log.Error(method, err)
		return
	}

	// Create user account if not found
	if err == sql.ErrNoRows {
		h.Runtime.Log.Info("Adding new LDAP user " + lu.Email + " @ " + dom)

		u = convertUser(lc, lu)
		u.Salt = secrets.GenerateSalt()
		u.Password = secrets.GeneratePassword(secrets.GenerateRandomPassword(), u.Salt)

		u, err = auth.AddExternalUser(ctx, h.Runtime, h.Store, u, lc.DefaultPermissionAddSpace)
		if err != nil {
			response.WriteServerError(w, method, err)
			h.Runtime.Log.Error(method, err)
			return
		}
	}

	// Attach user accounts and work out permissions.
	usr.AttachUserAccounts(ctx, *h.Store, org.RefID, &u)

	// No accounts signals data integrity problem
	// so we reject login request.
	if len(u.Accounts) == 0 {
		response.WriteUnauthorizedError(w)
		h.Runtime.Log.Error(method, err)
		return
	}

	// Abort login request if account is disabled.
	for _, ac := range u.Accounts {
		if ac.OrgID == org.RefID {
			if ac.Active == false {
				response.WriteUnauthorizedError(w)
				h.Runtime.Log.Error(method, err)
				return
			}
			break
		}
	}

	// Generate JWT token
	authModel := ath.AuthenticationModel{}
	authModel.Token = auth.GenerateJWT(h.Runtime, u.RefID, org.RefID, dom)
	authModel.User = u

	response.WriteJSON(w, authModel)
}
