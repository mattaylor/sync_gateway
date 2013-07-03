//  Copyright (c) 2012 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
//  except in compliance with the License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing, software distributed under the
//  License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
//  either express or implied. See the License for the specific language governing permissions
//  and limitations under the License.

package rest

import (
	"encoding/json"
	"io/ioutil"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/couchbaselabs/sync_gateway/auth"
	"github.com/couchbaselabs/sync_gateway/base"
	ch "github.com/couchbaselabs/sync_gateway/channels"
)

//////// DATABASE MAINTENANCE:

// "Create" a database (actually just register an existing bucket)
func (h *handler) handleCreateDB() error {
	h.assertAdminOnly()
	dbName := h.PathVars()["newdb"]
	var config *DbConfig
	if err := h.readJSONInto(&config); err != nil {
		return err
	}
	if config.name == "" {
		config.name = dbName
	} else if config.name != dbName {
		return &base.HTTPError{http.StatusBadRequest, "name mismatch"}
	}
	if err := h.server.addDatabaseFromConfig(config); err != nil {
		return err
	}
	return &base.HTTPError{http.StatusCreated, "created"}
}

// "Delete" a database (it doesn't actually do anything to the underlying bucket)
func (h *handler) handleDeleteDB() error {
	h.assertAdminOnly()
	if !h.server.removeDatabase(h.db.Name) {
		return &base.HTTPError{http.StatusNotFound, "missing"}
	}
	return nil
}

//////// USERS & ROLES:

func internalUserName(name string) string {
	if name == "GUEST" {
		return ""
	}
	return name
}

func externalUserName(name string) string {
	if name == "" {
		return "GUEST"
	}
	return name
}

// Public serialization of User/Role as used in the admin REST API.
type PrincipalJSON struct {
	Name              *string  `json:"name,omitempty"`
	ExplicitChannels  base.Set `json:"admin_channels,omitempty"`
	Channels          base.Set `json:"all_channels"`
	Email             string   `json:"email,omitempty"`
	Disabled          bool     `json:"disabled,omitempty"`
	Password          *string  `json:"password,omitempty"`
	ExplicitRoleNames []string `json:"admin_roles,omitempty"`
	RoleNames         []string `json:"roles,omitempty"`
}

func marshalPrincipal(princ auth.Principal) ([]byte, error) {
	name := externalUserName(princ.Name())
	info := PrincipalJSON{
		Name:             &name,
		ExplicitChannels: princ.ExplicitChannels().AsSet(),
		Channels:         princ.Channels().AsSet(),
	}
	if user, ok := princ.(auth.User); ok {
		info.Email = user.Email()
		info.Disabled = user.Disabled()
		info.ExplicitRoleNames = user.ExplicitRoleNames()
		info.RoleNames = user.RoleNames()
	}
	return json.Marshal(info)
}

// Handles PUT and POST for a user or a role.
func (h *handler) updatePrincipal(name string, isUser bool) error {
	h.assertAdminOnly()
	// Unmarshal the request body into a PrincipalJSON struct:
	body, _ := ioutil.ReadAll(h.rq.Body)
	var newInfo PrincipalJSON
	var err error
	if err = json.Unmarshal(body, &newInfo); err != nil {
		return err
	}

	var princ auth.Principal
	var user auth.User
	if h.rq.Method == "POST" {
		// On POST, take the name from the "name" property in the request body:
		if newInfo.Name == nil {
			return &base.HTTPError{http.StatusBadRequest, "Missing name property"}
		}
		name = *newInfo.Name
	} else {
		// ON PUT, get the existing user/role (if any):
		if newInfo.Name != nil && *newInfo.Name != name {
			return &base.HTTPError{http.StatusBadRequest, "Name mismatch (can't change name)"}
		}
		if isUser {
			user, err = h.context.auth.GetUser(internalUserName(name))
			princ = user
		} else {
			princ, err = h.context.auth.GetRole(name)
		}
		if err != nil {
			return err
		}
	}

	if princ == nil {
		// If user/role didn't exist already, instantiate a new one:
		if isUser {
			user, err = h.context.auth.NewUser(internalUserName(name), "", nil)
			princ = user
		} else {
			princ, err = h.context.auth.NewRole(name, nil)
		}
		if err != nil {
			return err
		}
	}

	// workaround for issue #99
	if princ.ExplicitChannels() == nil {
		newSet := make(ch.TimedSet, 0)
		princ.SetExplicitChannels(newSet)
	}

	// Now update the Principal object from the properties in the request:
	princ.ExplicitChannels().UpdateAtSequence(newInfo.ExplicitChannels,
		h.context.dbcontext.LastSequence()+1)
	if isUser {
		user.SetEmail(newInfo.Email)
		if newInfo.Password != nil {
			user.SetPassword(*newInfo.Password)
		}
		user.SetDisabled(newInfo.Disabled)
		user.SetExplicitRoleNames(newInfo.ExplicitRoleNames)
	}

	// And finally save the Principal:
	if err = h.context.auth.Save(princ); err != nil {
		return err
	}
	h.response.WriteHeader(http.StatusCreated)
	return nil
}

// Handles PUT or POST to /_user/*
func (h *handler) putUser() error {
	username := mux.Vars(h.rq)["name"]
	return h.updatePrincipal(username, true)
}

// Handles PUT or POST to /_role/*
func (h *handler) putRole() error {
	rolename := mux.Vars(h.rq)["name"]
	return h.updatePrincipal(rolename, false)
}

func (h *handler) deleteUser() error {
	h.assertAdminOnly()
	user, err := h.context.auth.GetUser(mux.Vars(h.rq)["name"])
	if user == nil {
		if err == nil {
			err = kNotFoundError
		}
		return err
	}
	return h.context.auth.Delete(user)
}

func (h *handler) deleteRole() error {
	h.assertAdminOnly()
	role, err := h.context.auth.GetRole(mux.Vars(h.rq)["name"])
	if role == nil {
		if err == nil {
			err = kNotFoundError
		}
		return err
	}
	return h.context.auth.Delete(role)
}

func (h *handler) getUserInfo() error {
	h.assertAdminOnly()
	user, err := h.context.auth.GetUser(internalUserName(mux.Vars(h.rq)["name"]))
	if user == nil {
		if err == nil {
			err = kNotFoundError
		}
		return err
	}

	bytes, err := marshalPrincipal(user)
	h.response.Write(bytes)
	return err
}

func (h *handler) getRoleInfo() error {
	h.assertAdminOnly()
	role, err := h.context.auth.GetRole(mux.Vars(h.rq)["name"])
	if role == nil {
		if err == nil {
			err = kNotFoundError
		}
		return err
	}
	bytes, err := marshalPrincipal(role)
	h.response.Write(bytes)
	return err
}

func (h *handler) getUsers() error {
	users, _, err := h.context.dbcontext.AllPrincipalIDs()
	if err != nil {
		return err
	}
	bytes, err := json.Marshal(users)
	h.response.Write(bytes)
	return err
}

func (h *handler) getRoles() error {
	_, roles, err := h.context.dbcontext.AllPrincipalIDs()
	if err != nil {
		return err
	}
	bytes, err := json.Marshal(roles)
	h.response.Write(bytes)
	return err
}
