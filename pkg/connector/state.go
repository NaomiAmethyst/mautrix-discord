// SPDX-License-Identifier: AGPL-3.0-or-later
package connector

import "github.com/bwmarrin/discordgo"

// discordgo returns mutable state pointers. Copy the fields we consume while
// holding its state lock so gateway updates cannot race portal/API work.
func copyChannel(ch *discordgo.Channel) *discordgo.Channel {
	copy := *ch
	copy.Recipients = make([]*discordgo.User, len(ch.Recipients))
	for i, user := range ch.Recipients {
		u := *user
		copy.Recipients[i] = &u
	}
	if ch.ThreadMetadata != nil {
		meta := *ch.ThreadMetadata
		copy.ThreadMetadata = &meta
	}
	return &copy
}

func cachedChannel(state *discordgo.State, cid string) (*discordgo.Channel, error) {
	ch, err := state.Channel(cid)
	if err != nil {
		return nil, err
	}
	state.RLock()
	defer state.RUnlock()
	return copyChannel(ch), nil
}

func copyGuild(g *discordgo.Guild) *discordgo.Guild {
	copy := *g
	copy.Channels = make([]*discordgo.Channel, len(g.Channels))
	for i, ch := range g.Channels {
		copy.Channels[i] = copyChannel(ch)
	}
	copy.Threads = make([]*discordgo.Channel, len(g.Threads))
	for i, ch := range g.Threads {
		copy.Threads[i] = copyChannel(ch)
	}
	copy.Roles = make([]*discordgo.Role, len(g.Roles))
	for i, role := range g.Roles {
		r := *role
		copy.Roles[i] = &r
	}
	return &copy
}

func snapshotGuild(state *discordgo.State, g *discordgo.Guild) *discordgo.Guild {
	state.RLock()
	defer state.RUnlock()
	return copyGuild(g)
}

func cachedGuild(state *discordgo.State, gid string) (*discordgo.Guild, error) {
	g, err := state.Guild(gid)
	if err != nil {
		return nil, err
	}
	return snapshotGuild(state, g), nil
}

func snapshotReady(state *discordgo.State, ready *discordgo.Ready) *discordgo.Ready {
	state.RLock()
	defer state.RUnlock()
	copy := *ready
	copy.Guilds = make([]*discordgo.Guild, len(ready.Guilds))
	for i, g := range ready.Guilds {
		copy.Guilds[i] = copyGuild(g)
	}
	copy.PrivateChannels = make([]*discordgo.Channel, len(ready.PrivateChannels))
	for i, ch := range ready.PrivateChannels {
		copy.PrivateChannels[i] = copyChannel(ch)
	}
	return &copy
}

// The SDK permission helper reads cached pointers after releasing their locks.
// Run it against a private snapshot during guild discovery.
func permissionsState(state *discordgo.State, guild *discordgo.Guild, uid string) *discordgo.State {
	copy := *guild
	copy.Members = nil
	if member, err := state.Member(guild.ID, uid); err == nil {
		state.RLock()
		m := *member
		m.Roles = append([]string(nil), member.Roles...)
		if member.User != nil {
			u := *member.User
			m.User = &u
		}
		state.RUnlock()
		if m.User != nil {
			copy.Members = []*discordgo.Member{&m}
		}
	}
	result := discordgo.NewState()
	_ = result.GuildAdd(&copy)
	return result
}
