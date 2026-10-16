package access

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

const (
	view = discordgo.PermissionViewChannel
	send = discordgo.PermissionSendMessages
)

// testGuild：@everyone 角色（ID 與伺服器相同）有 everyone 權限；其他角色依 roles。
func testGuild(everyone int64, roles ...*discordgo.Role) *discordgo.Guild {
	g := &discordgo.Guild{ID: "g", OwnerID: "owner", Roles: []*discordgo.Role{{ID: "g", Permissions: everyone}}}
	g.Roles = append(g.Roles, roles...)
	return g
}

func testChannel(ows ...*discordgo.PermissionOverwrite) *discordgo.Channel {
	return &discordgo.Channel{ID: "c", GuildID: "g", PermissionOverwrites: ows}
}

func roleOW(id string, allow, deny int64) *discordgo.PermissionOverwrite {
	return &discordgo.PermissionOverwrite{ID: id, Type: discordgo.PermissionOverwriteTypeRole, Allow: allow, Deny: deny}
}

func memberOW(id string, allow, deny int64) *discordgo.PermissionOverwrite {
	return &discordgo.PermissionOverwrite{ID: id, Type: discordgo.PermissionOverwriteTypeMember, Allow: allow, Deny: deny}
}

func has(p, perm int64) bool { return p&perm == perm }

func TestPerms_Owner(t *testing.T) {
	g := testGuild(0)
	ch := testChannel(roleOW("g", 0, view), memberOW("owner", 0, view))
	if p := ChannelPermissions(g, ch, "owner", nil); p != discordgo.PermissionAll {
		t.Errorf("擁有者應有全部權限，得到 %b", p)
	}
}

func TestPerms_Admin(t *testing.T) {
	g := testGuild(0, &discordgo.Role{ID: "admin", Permissions: discordgo.PermissionAdministrator})
	ch := testChannel(roleOW("g", 0, view), roleOW("admin", 0, view), memberOW("u", 0, view))
	if p := ChannelPermissions(g, ch, "u", []string{"admin"}); !has(p, view) || !has(p, discordgo.PermissionAll) {
		t.Errorf("Administrator 不受覆寫影響，應有全部權限，得到 %b", p)
	}
	// @everyone 有 Administrator 也一樣。
	if p := ChannelPermissions(testGuild(discordgo.PermissionAdministrator), ch, "u", nil); !has(p, view) {
		t.Errorf("@everyone 為 Administrator 時應可檢視，得到 %b", p)
	}
}

func TestPerms_BaseFromEveryoneAndRoles(t *testing.T) {
	g := testGuild(view, &discordgo.Role{ID: "r1", Permissions: send}, &discordgo.Role{ID: "r2", Permissions: discordgo.PermissionAttachFiles})
	p := ChannelPermissions(g, testChannel(), "u", []string{"r1"})
	if !has(p, view|send) || has(p, discordgo.PermissionAttachFiles) {
		t.Errorf("應為 @everyone 與成員角色權限的聯集（不含未擁有的角色），得到 %b", p)
	}
	if p := ChannelPermissions(testGuild(0), testChannel(), "u", nil); has(p, view) {
		t.Errorf("沒有任何權限時不可檢視，得到 %b", p)
	}
}

func TestPerms_EveryoneDeniedRoleAllowed(t *testing.T) {
	g := testGuild(view, &discordgo.Role{ID: "r", Permissions: 0})
	ch := testChannel(roleOW("g", 0, view), roleOW("r", view, 0))
	if p := ChannelPermissions(g, ch, "u", []string{"r"}); !has(p, view) {
		t.Errorf("角色覆寫允許應蓋過 @everyone 拒絕，得到 %b", p)
	}
	if p := ChannelPermissions(g, ch, "other", nil); has(p, view) {
		t.Errorf("沒有該角色的人應被 @everyone 覆寫拒絕，得到 %b", p)
	}
}

func TestPerms_RoleDenyEveryoneAllow(t *testing.T) {
	g := testGuild(0, &discordgo.Role{ID: "r"})
	ch := testChannel(roleOW("g", view, 0), roleOW("r", 0, view))
	if p := ChannelPermissions(g, ch, "u", []string{"r"}); has(p, view) {
		t.Errorf("角色覆寫拒絕應蓋過 @everyone 覆寫允許，得到 %b", p)
	}
}

// 多個角色覆寫：先合併所有 deny 再合併所有 allow，allow 優先。
func TestPerms_RoleOverwritesAllowWinsOverDeny(t *testing.T) {
	g := testGuild(view, &discordgo.Role{ID: "r1"}, &discordgo.Role{ID: "r2"})
	ch := testChannel(roleOW("r1", 0, view), roleOW("r2", view, 0))
	if p := ChannelPermissions(g, ch, "u", []string{"r1", "r2"}); !has(p, view) {
		t.Errorf("角色之間 allow 應優先於 deny，得到 %b", p)
	}
	if p := ChannelPermissions(g, ch, "u", []string{"r1"}); has(p, view) {
		t.Errorf("只有拒絕的角色應不可檢視，得到 %b", p)
	}
}

func TestPerms_MemberOverrideDeny(t *testing.T) {
	g := testGuild(view, &discordgo.Role{ID: "r"})
	ch := testChannel(roleOW("r", view, 0), memberOW("u", 0, view))
	if p := ChannelPermissions(g, ch, "u", []string{"r"}); has(p, view) {
		t.Errorf("成員覆寫拒絕應蓋過角色允許，得到 %b", p)
	}
	if p := ChannelPermissions(g, ch, "u2", []string{"r"}); !has(p, view) {
		t.Errorf("其他成員不受影響，得到 %b", p)
	}
}

func TestPerms_MemberOverrideAllow(t *testing.T) {
	g := testGuild(0, &discordgo.Role{ID: "r"})
	ch := testChannel(roleOW("g", 0, view), roleOW("r", 0, view), memberOW("u", view, 0))
	if p := ChannelPermissions(g, ch, "u", []string{"r"}); !has(p, view) {
		t.Errorf("成員覆寫允許應蓋過角色與 @everyone 拒絕，得到 %b", p)
	}
}

// 角色 ID 與成員 ID 相同時，以覆寫類型區分（角色覆寫不套用到同 ID 的成員）。
func TestPerms_OverwriteTypeDistinguished(t *testing.T) {
	g := testGuild(view)
	ch := testChannel(roleOW("u", 0, view))
	if p := ChannelPermissions(g, ch, "u", nil); !has(p, view) {
		t.Errorf("角色覆寫不應套用到同 ID 的成員，得到 %b", p)
	}
	ch = testChannel(memberOW("r", 0, view))
	g = testGuild(view, &discordgo.Role{ID: "r"})
	if p := ChannelPermissions(g, ch, "u", []string{"r"}); !has(p, view) {
		t.Errorf("成員覆寫不應套用到同 ID 的角色，得到 %b", p)
	}
}

func TestPerms_NilSafe(t *testing.T) {
	if p := ChannelPermissions(nil, testChannel(), "u", nil); p != 0 {
		t.Errorf("沒有伺服器資料應無權限，得到 %b", p)
	}
	if p := ChannelPermissions(testGuild(view), nil, "u", nil); p != 0 {
		t.Errorf("沒有頻道資料應無權限，得到 %b", p)
	}
	g := testGuild(view)
	g.Roles = append(g.Roles, nil)
	ch := testChannel(nil, roleOW("g", 0, 0))
	if p := ChannelPermissions(g, ch, "u", nil); !has(p, view) {
		t.Errorf("nil 項目應略過，得到 %b", p)
	}
}
