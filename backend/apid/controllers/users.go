package controllers

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"github.com/sensu/sensu-go/backend/authorization"
	"github.com/sensu/sensu-go/backend/store"
	"github.com/sensu/sensu-go/types"
)

// UsersController defines the fields required by UsersController.
type UsersController struct {
	Store interface {
		store.UserStore
		store.RBACStore
	}
}

// Register should define an association between HTTP routes and their
// respective handlers defined within this Controller.
func (c *UsersController) Register(r *mux.Router) {
	r.HandleFunc("/rbac/users", c.many).Methods(http.MethodGet)
	r.HandleFunc("/rbac/users", c.updateUser).Methods(http.MethodPut)
	r.HandleFunc("/rbac/users/{username}", c.single).Methods(http.MethodGet)
	r.HandleFunc("/rbac/users/{username}", c.deleteUser).Methods(http.MethodDelete)

	// TODO (JP):
	// Lot of duplication between single, password & reinstate. Could probably
	// be combined.
	r.HandleFunc("/rbac/users/{username}/password", c.password).Methods(http.MethodPut)
	r.HandleFunc("/rbac/users/{username}/reinstate", c.reinstate).Methods(http.MethodPut)

	// Add/Remove Roles
	r.HandleFunc("/rbac/users/{username}/roles/{role}", c.roles).Methods(http.MethodPut, http.MethodDelete)
}

// deleteUser handles DELETE requests to /users/:username
func (c *UsersController) deleteUser(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	user := types.User{Username: vars["username"]}

	abilities := authorization.Users.WithContext(r.Context())
	if !abilities.CanDelete(&user) {
		authorization.UnauthorizedAccessToResource(w)
		return
	}

	if err := c.Store.DeleteUserByName(user.Username); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	return
}

// many handles GET requests to /users
func (c *UsersController) many(w http.ResponseWriter, r *http.Request) {
	abilities := authorization.Users.WithContext(r.Context())
	if !abilities.CanList() {
		authorization.UnauthorizedAccessToResource(w)
		return
	}

	users, err := c.Store.GetAllUsers()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Reject those resources the viewer is unauthorized to view
	rejectUsers(&users, abilities.CanRead)

	// Obfustace users password for security
	for i := range users {
		users[i].Password = ""
	}

	usersBytes, err := json.Marshal(users)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, string(usersBytes))
}

// single handles requests to /users/:username
func (c *UsersController) single(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	username := vars["username"]

	var (
		user *types.User
		err  error
	)

	user, err = c.Store.GetUser(username)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if user == nil {
		http.NotFound(w, r)
		return
	}

	abilities := authorization.Users.WithContext(r.Context())
	if !abilities.CanRead(user) {
		authorization.UnauthorizedAccessToResource(w)
		return
	}

	// Obfustace user password for security
	user.Password = ""

	userBytes, err := json.Marshal(user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, string(userBytes))

}

func (c *UsersController) updateUser(w http.ResponseWriter, r *http.Request) {
	var user types.User

	bodyBytes, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	err = json.Unmarshal(bodyBytes, &user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	isCreate := false
	if u, _ := c.Store.GetUser(user.Username); u == nil {
		isCreate = true
	}

	abilities := authorization.Users.WithContext(r.Context())
	switch {
	case isCreate && !abilities.CanCreate():
		fallthrough
	case !isCreate && !abilities.CanUpdate(&user):
		authorization.UnauthorizedAccessToResource(w)
		return
	}

	err = user.Validate()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	err = user.ValidatePassword()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	err = validateRoles(c.Store, user.Roles)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	err = c.Store.CreateUser(&user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	return
}

func (c *UsersController) password(w http.ResponseWriter, r *http.Request) {
	var user *types.User
	abilities := authorization.Users.WithContext(r.Context())

	bodyBytes, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	params := map[string]string{}
	err = json.Unmarshal(bodyBytes, &params)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	vars := mux.Vars(r)
	if user, err = c.Store.GetUser(vars["username"]); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if user == nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	if !abilities.CanUpdate(user) {
		authorization.UnauthorizedAccessToResource(w)
		return
	}

	user.Password = params["password"]
	if err = user.ValidatePassword(); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}

	if err = c.Store.UpdateUser(user); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	return
}

func (c *UsersController) reinstate(w http.ResponseWriter, r *http.Request) {
	var user *types.User
	var err error

	vars := mux.Vars(r)
	abilities := authorization.Users.WithContext(r.Context())

	if user, err = c.Store.GetUser(vars["username"]); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !abilities.CanUpdate(user) {
		authorization.UnauthorizedAccessToResource(w)
		return
	}

	user.Disabled = false
	if err = c.Store.UpdateUser(user); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	return
}

func (c *UsersController) roles(w http.ResponseWriter, r *http.Request) {
	var user *types.User
	var err error

	vars := mux.Vars(r)
	role := vars["role"]
	abilities := authorization.Users.WithContext(r.Context())

	if user, err = c.Store.GetUser(vars["username"]); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !abilities.CanUpdate(user) {
		authorization.UnauthorizedAccessToResource(w)
		return
	}

	switch r.Method {
	case http.MethodPut:
		var exists bool
		for _, r := range user.Roles {
			if r == role {
				exists = true
				break
			}
		}

		if !exists {
			user.Roles = append(user.Roles, role)
		}
	case http.MethodDelete:
		newRoles := []string{}
		for _, r := range user.Roles {
			if r != role {
				newRoles = append(newRoles, r)
			}
		}

		user.Roles = newRoles
	}

	if err = validateRoles(c.Store, user.Roles); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}

	if err = c.Store.UpdateUser(user); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	return
}

func validateRoles(store store.RBACStore, givenRoles []string) error {
	storedRoles, err := store.GetRoles()
	if err != nil {
		return err
	}

	missingRoles := []string{}

	for _, givenRole := range givenRoles {
		if present := hasRole(storedRoles, givenRole); !present {
			missingRoles = append(missingRoles, givenRole)
		}
	}

	if len(missingRoles) != 0 {
		message := "not exist and should be created first"
		if len(missingRoles) == 1 {
			message = fmt.Sprintf("given role '%s' does %s", missingRoles[0], message)
		} else {
			message = fmt.Sprintf(
				"given roles '%s' do %s",
				strings.Join(missingRoles, ", "),
				message,
			)
		}
		return fmt.Errorf(message)
	}

	return nil
}

func hasRole(roles []*types.Role, roleName string) bool {
	for _, role := range roles {
		if roleName == role.Name {
			return true
		}
	}
	return false
}

func rejectUsers(records *[]*types.User, predicate func(*types.User) bool) {
	for i := 0; i < len(*records); i++ {
		if !predicate((*records)[i]) {
			*records = append((*records)[:i], (*records)[i+1:]...)
			i--
		}
	}
}
