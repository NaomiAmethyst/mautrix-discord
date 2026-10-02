// SPDX-License-Identifier: AGPL-3.0-or-later
package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/status"
)

func (d *DiscordConnector) requestClient(prov bridgev2.IProvisioningAPI, r *http.Request) (*DiscordClient, error) {
	user := prov.GetUser(r)
	login := user.GetDefaultLogin()
	if login == nil {
		return nil, fmt.Errorf("not logged in")
	}
	c, ok := login.Client.(*DiscordClient)
	if !ok || !c.IsLoggedIn() {
		return nil, fmt.Errorf("Discord account unavailable")
	}
	return c, nil
}
func (d *DiscordConnector) registerLegacyProvisioning(prov bridgev2.IProvisioningAPI) {
	router := prov.GetRouter()
	router.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) {
		user := prov.GetUser(r)
		var account map[string]any = map[string]any{"logged_in": false, "connected": false, "conn": map[string]any{}}
		if c, err := d.requestClient(prov, r); err == nil {
			connected := c.gatewayConnected.Load()
			account["id"], account["logged_in"], account["connected"] = c.UserLogin.ID, c.IsLoggedIn(), connected
		}
		writeJSON(w, 200, map[string]any{"mxid": user.MXID, "management_room": user.ManagementRoom, "Discord": account})
	})
	for _, action := range []string{"disconnect", "reconnect", "logout"} {
		router.HandleFunc("POST /v1/"+action, func(w http.ResponseWriter, r *http.Request) {
			c, err := d.requestClient(prov, r)
			if err != nil {
				writeError(w, 404, "M_NOT_FOUND", err.Error())
				return
			}
			switch action {
			case "disconnect":
				c.Disconnect()
			case "reconnect":
				c.Disconnect()
				go c.Connect(d.Bridge.BackgroundCtx)
			case "logout":
				c.UserLogin.Delete(r.Context(), status.BridgeState{StateEvent: status.StateLoggedOut}, bridgev2.DeleteOpts{LogoutRemote: true})
			}
			writeJSON(w, 200, map[string]any{"success": true, "status": action + " successful"})
		})
	}
	router.HandleFunc("POST /v1/login/token", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token string `json:"token"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req) != nil {
			writeError(w, 400, "M_BAD_JSON", "Expected token")
			return
		}
		flow := "user-token"
		token := strings.TrimSpace(req.Token)
		if strings.HasPrefix(token, "Bot ") {
			flow = "bot-token"
		} else if strings.HasPrefix(token, "Bearer ") {
			flow = "oauth-token"
		}
		process, err := d.CreateLogin(r.Context(), prov.GetUser(r), flow)
		if err != nil {
			writeError(w, 403, "M_FORBIDDEN", "Login flow is unavailable or not permitted")
			return
		}
		defer process.Cancel()
		step, err := process.(*TokenLogin).SubmitUserInput(r.Context(), map[string]string{"token": token})
		if err != nil {
			writeError(w, 401, "FI.MAU.DISCORD.LOGIN_FAILED", "Discord rejected login")
			return
		}
		writeJSON(w, 200, map[string]any{"success": true, "id": step.CompleteParams.UserLoginID, "username": step.CompleteParams.UserLogin.RemoteName, "discriminator": "0"})
	})
	router.HandleFunc("GET /v1/login/qr", func(w http.ResponseWriter, r *http.Request) {
		process, err := d.CreateLogin(r.Context(), prov.GetUser(r), "qr")
		if err != nil {
			writeError(w, 403, "M_FORBIDDEN", "QR login is unavailable or not permitted")
			return
		}
		defer process.Cancel()
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		ctx = conn.CloseRead(ctx)
		write := func(value any) error {
			raw, _ := json.Marshal(value)
			return conn.Write(ctx, websocket.MessageText, raw)
		}
		step, err := process.Start(ctx)
		if err != nil {
			_ = write(map[string]any{"error": "Failed to start QR login", "errcode": "FI.MAU.DISCORD.LOGIN_FAILED"})
			return
		}
		if write(map[string]any{"code": step.DisplayAndWaitParams.Data, "timeout": 300}) != nil {
			return
		}
		step, err = process.(*QRLogin).Wait(ctx)
		if err != nil {
			_ = write(map[string]any{"error": "QR login failed", "errcode": "FI.MAU.DISCORD.LOGIN_FAILED"})
			return
		}
		_ = write(map[string]any{"success": true, "id": step.CompleteParams.UserLoginID, "username": step.CompleteParams.UserLogin.RemoteName, "discriminator": "0"})
	})
	for _, path := range []string{"/v1/guilds", "/v3/discord/guilds"} {
		router.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			c, err := d.requestClient(prov, r)
			if err != nil {
				writeError(w, 404, "M_NOT_FOUND", err.Error())
				return
			}
			writeJSON(w, 200, map[string]any{"guilds": c.guildEntries(r.Context())})
		})
	}
	for _, path := range []string{"/v1/guilds/{guildID}", "/v3/discord/guilds/{guildID}"} {
		router.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			c, err := d.requestClient(prov, r)
			if err != nil {
				writeError(w, 404, "M_NOT_FOUND", err.Error())
				return
			}
			if !c.Session.IsUser && !prov.GetUser(r).Permissions.Admin {
				writeError(w, 403, "M_FORBIDDEN", "Only admins may configure bot guilds")
				return
			}
			var req struct {
				AutoCreateChannels bool   `json:"auto_create_channels"`
				Mode               string `json:"mode"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil {
				writeError(w, 400, "M_BAD_JSON", "Invalid guild request")
				return
			}
			mode := req.Mode
			if mode == "" {
				mode = "create-on-message"
				if req.AutoCreateChannels {
					mode = "everything"
				}
			}
			gid := r.PathValue("guildID")
			if err = c.setGuildMode(r.Context(), gid, mode); err != nil {
				writeError(w, 409, "FI.MAU.DISCORD.GUILD_FAILED", err.Error())
				return
			}
			p, _ := d.Bridge.DB.Portal.GetByKey(r.Context(), c.portalKey("guild:"+gid))
			mxid := ""
			if p != nil {
				mxid = string(p.MXID)
			}
			writeJSON(w, 200, map[string]any{"success": true, "mxid": mxid, "bridging_mode": mode})
		})
		router.HandleFunc("DELETE "+path, func(w http.ResponseWriter, r *http.Request) {
			if !prov.GetUser(r).Permissions.Admin {
				writeError(w, 403, "M_FORBIDDEN", "Bridge admin permission required")
				return
			}
			c, err := d.requestClient(prov, r)
			if err == nil {
				err = c.unbridgeGuild(r.Context(), r.PathValue("guildID"), false)
			}
			if err != nil {
				writeError(w, 409, "FI.MAU.DISCORD.GUILD_FAILED", err.Error())
				return
			}
			writeJSON(w, 200, map[string]any{"success": true, "status": "Guild unbridged"})
		})
	}
}
