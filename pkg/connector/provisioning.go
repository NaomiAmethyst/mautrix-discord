// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

// Routes are installed inside the central provisioning router, so its existing
// bearer/Matrix authentication and user permission checks protect every route.
func (d *DiscordConnector) RegisterProvisioning(prov matrix.IProvisioningAPI) {
	if prov == nil {
		return
	}
	d.registerLegacyProvisioning(prov)
	prov.GetRouter().HandleFunc("GET /v3/discord/channels", func(w http.ResponseWriter, r *http.Request) {
		if !prov.GetUser(r).Permissions.Admin {
			writeError(w, http.StatusForbidden, "M_FORBIDDEN", "Bridge admin permission required")
			return
		}
		type channel struct {
			ID           string                `json:"id"`
			RoomID       id.RoomID             `json:"room_id,omitempty"`
			RelayLoginID networkid.UserLoginID `json:"relay_login_id,omitempty"`
		}
		response := make([]channel, 0, len(d.Config.Channels))
		for _, ch := range d.Config.Channels {
			entry := channel{ID: ch.ID}
			portal, err := d.Bridge.GetExistingPortalByKey(r.Context(), networkid.PortalKey{ID: networkid.PortalID(ch.ID)})
			if err != nil {
				writeError(w, http.StatusInternalServerError, "M_UNKNOWN", "Failed to load channels")
				return
			}
			if portal != nil {
				entry.RoomID = portal.MXID
				entry.RelayLoginID = portal.RelayLoginID
			}
			response = append(response, entry)
		}
		writeJSON(w, http.StatusOK, map[string]any{"channels": response})
	})
	prov.GetRouter().HandleFunc("POST /v3/discord/channels/{channelID}/bridge", func(w http.ResponseWriter, r *http.Request) {
		user := prov.GetUser(r)
		if !user.Permissions.Admin {
			writeError(w, http.StatusForbidden, "M_FORBIDDEN", "Bridge admin permission required")
			return
		}
		channelID := r.PathValue("channelID")
		if d.Config.channel(channelID) == nil {
			writeError(w, http.StatusForbidden, "M_FORBIDDEN", "Channel is not in network.channels")
			return
		}
		var request struct {
			RoomID  id.RoomID             `json:"room_id"`
			LoginID networkid.UserLoginID `json:"login_id"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil || !strings.HasPrefix(string(request.RoomID), "!") || !strings.Contains(string(request.RoomID), ":") || request.LoginID == "" {
			writeError(w, http.StatusBadRequest, "M_BAD_JSON", "Expected room_id and login_id")
			return
		}
		if decoder.Decode(&struct{}{}) != io.EOF {
			writeError(w, http.StatusBadRequest, "M_BAD_JSON", "Expected one JSON object")
			return
		}
		login, err := d.Bridge.GetExistingUserLoginByID(r.Context(), request.LoginID)
		if err != nil || login == nil {
			writeError(w, http.StatusNotFound, "M_NOT_FOUND", "Login not found")
			return
		}
		client, ok := login.Client.(*DiscordClient)
		if !ok || !client.IsLoggedIn() {
			writeError(w, http.StatusConflict, "M_UNKNOWN", "Discord login is unavailable")
			return
		}
		if !client.allowed(channelID) {
			writeError(w, http.StatusForbidden, "M_FORBIDDEN", "Channel is assigned to a different relay login")
			return
		}
		portal, err := d.bindChannel(r.Context(), client, channelID, request.RoomID)
		if err != nil {
			writeError(w, http.StatusConflict, "FI.MAU.DISCORD.BIND_FAILED", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"room_id": portal.MXID, "channel_id": channelID, "relay_login_id": login.ID})
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"errcode": code, "error": message})
}

func (d *DiscordConnector) adminLogin(ctxUser *bridgev2.User, loginID string) (*DiscordClient, error) {
	if !ctxUser.Permissions.Admin {
		return nil, fmt.Errorf("bridge admin permission required")
	}
	if loginID == "" {
		if login := ctxUser.GetDefaultLogin(); login != nil {
			if c, ok := login.Client.(*DiscordClient); ok {
				return c, nil
			}
		}
		return nil, fmt.Errorf("log in to Discord first")
	}
	login := d.Bridge.GetCachedUserLoginByID(networkid.UserLoginID(loginID))
	if login == nil {
		return nil, fmt.Errorf("login not found")
	}
	c, ok := login.Client.(*DiscordClient)
	if !ok || !c.IsLoggedIn() {
		return nil, fmt.Errorf("login is unavailable")
	}
	return c, nil
}
