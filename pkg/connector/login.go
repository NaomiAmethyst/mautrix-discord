// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/mautrix-discord/pkg/remoteauth"

	"github.com/bwmarrin/discordgo"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

func (d *DiscordConnector) GetLoginFlows() []bridgev2.LoginFlow {
	flows := []bridgev2.LoginFlow{{ID: "bot-token", Name: "Discord bot token", Description: "Connect an admin-managed Discord bot for webhook relay"}}
	if d.Config.PersonalAccounts {
		flows = append(flows, bridgev2.LoginFlow{ID: "user-token", Name: "Discord account token"}, bridgev2.LoginFlow{ID: "oauth-token", Name: "Discord OAuth token"}, bridgev2.LoginFlow{ID: "qr", Name: "Scan a QR code with Discord"})
	}
	return flows
}
func (d *DiscordConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	if flowID == "bot-token" {
		if !user.Permissions.Admin {
			return nil, bridgev2.RespError{StatusCode: http.StatusForbidden, ErrCode: "M_FORBIDDEN", Err: "Only bridge admins may configure a relay bot"}
		}
		return &TokenLogin{Main: d, User: user, AccountType: "bot"}, nil
	}
	if !d.Config.PersonalAccounts || !user.Permissions.Login {
		return nil, bridgev2.ErrInvalidLoginFlowID
	}
	switch flowID {
	case "user-token":
		return &TokenLogin{Main: d, User: user, AccountType: "user"}, nil
	case "oauth-token":
		return &TokenLogin{Main: d, User: user, AccountType: "oauth"}, nil
	case "qr":
		return &QRLogin{Main: d, User: user}, nil
	default:
		return nil, bridgev2.ErrInvalidLoginFlowID
	}
}

type TokenLogin struct {
	AccountType string
	Main        *DiscordConnector
	User        *bridgev2.User
	cancelled   atomic.Bool
}

var _ bridgev2.LoginProcessUserInput = (*TokenLogin)(nil)

func (l *TokenLogin) Cancel() { l.cancelled.Store(true) }
func (l *TokenLogin) Start(context.Context) (*bridgev2.LoginStep, error) {
	name, step, instructions := "Bot token", "bot_token", "Enter your Discord bot token. Enable the Message Content intent in the Discord developer portal."
	if l.AccountType == "user" {
		name, step, instructions = "Account token", "user_token", "Enter your Discord account token"
	} else if l.AccountType == "oauth" {
		name, step, instructions = "OAuth token", "oauth_token", "Enter your Discord OAuth token"
	}
	return &bridgev2.LoginStep{Type: bridgev2.LoginStepTypeUserInput, StepID: "fi.mau.discord.login." + step, Instructions: instructions, UserInputParams: &bridgev2.LoginUserInputParams{Fields: []bridgev2.LoginInputDataField{{ID: "token", Type: bridgev2.LoginInputFieldTypeToken, Name: name}}}}, nil
}
func (l *TokenLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	if l.cancelled.Load() {
		return nil, fmt.Errorf("login cancelled")
	}
	token := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(input["token"]), "Bot "))
	if token == "" {
		return nil, fmt.Errorf("bot token is required")
	}
	accountType := l.AccountType
	if accountType == "" {
		accountType = "bot"
	}
	auth := token
	if accountType == "bot" {
		auth = "Bot " + token
	} else if accountType == "oauth" {
		auth = "Bearer " + strings.TrimPrefix(token, "Bearer ")
	}
	session, err := discordgo.New(auth)
	if err != nil {
		return nil, fmt.Errorf("invalid bot token")
	}
	session.Client = l.Main.HTTP
	user, err := session.User("@me", discordgo.WithContext(ctx))
	// Do not return Discord errors: webhook URLs and authorization headers may contain credentials.
	if err != nil || user == nil || (accountType == "bot" && !user.Bot) || (accountType != "bot" && user.Bot) {
		return nil, bridgev2.RespError{StatusCode: http.StatusUnauthorized, ErrCode: "M_UNAUTHORIZED", Err: "Discord rejected the account token"}
	}
	if l.cancelled.Load() {
		return nil, fmt.Errorf("login cancelled")
	}
	login, err := l.User.NewLogin(ctx, &database.UserLogin{ID: networkid.UserLoginID(user.ID), RemoteName: user.Username,
		Metadata: &LoginMetadata{Token: strings.TrimPrefix(token, "Bearer "), AccountType: accountType}}, &bridgev2.NewLoginParams{})
	if err != nil {
		return nil, err
	}
	go login.Client.Connect(l.Main.Bridge.BackgroundCtx)
	return &bridgev2.LoginStep{Type: bridgev2.LoginStepTypeComplete, StepID: "fi.mau.discord.login.complete", Instructions: "Discord account connected",
		CompleteParams: &bridgev2.LoginCompleteParams{UserLoginID: login.ID, UserLogin: login}}, nil
}

type QRLogin struct {
	Main      *DiscordConnector
	User      *bridgev2.User
	mu        sync.Mutex
	cancelled bool
	cancel    context.CancelFunc
	client    *remoteauth.Client
}

var _ bridgev2.LoginProcessDisplayAndWait = (*QRLogin)(nil)

func (q *QRLogin) Cancel() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.cancelled = true
	if q.cancel != nil {
		q.cancel()
	}
}
func (q *QRLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	q.mu.Lock()
	if q.cancelled || q.client != nil {
		q.mu.Unlock()
		return nil, fmt.Errorf("login cancelled or already started")
	}
	client, err := remoteauth.New(q.Main.HTTP)
	if err != nil {
		q.mu.Unlock()
		return nil, err
	}
	base := q.Main.Bridge.BackgroundCtx
	if base == nil {
		base = context.Background()
	}
	runCtx, cancel := context.WithTimeout(base, 5*time.Minute)
	q.client, q.cancel = client, cancel
	q.mu.Unlock()
	go client.Run(runCtx)
	select {
	case url := <-client.QR:
		return &bridgev2.LoginStep{Type: bridgev2.LoginStepTypeDisplayAndWait, StepID: "fi.mau.discord.login.qr", Instructions: "Scan this QR code in the Discord mobile app and confirm the login", DisplayAndWaitParams: &bridgev2.LoginDisplayAndWaitParams{Type: bridgev2.LoginDisplayTypeQR, Data: url}}, nil
	case <-client.Done:
		_, err := client.Result()
		q.Cancel()
		if err == nil {
			err = fmt.Errorf("QR login completed before displaying the challenge")
		}
		return nil, err
	case <-ctx.Done():
		q.Cancel()
		return nil, ctx.Err()
	}
}
func (q *QRLogin) Wait(ctx context.Context) (*bridgev2.LoginStep, error) {
	q.mu.Lock()
	client := q.client
	q.mu.Unlock()
	if client == nil {
		return nil, fmt.Errorf("QR login has not started")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-client.Done:
	}
	token, err := client.Result()
	if err != nil {
		return nil, err
	}
	q.mu.Lock()
	cancelled := q.cancelled
	q.mu.Unlock()
	if cancelled {
		return nil, fmt.Errorf("login cancelled")
	}
	return (&TokenLogin{Main: q.Main, User: q.User, AccountType: "user"}).SubmitUserInput(ctx, map[string]string{"token": token})
}
