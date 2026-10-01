package i18n

// enDict is the English admin UI dictionary. Keys mirror zhDict.
var enDict = map[string]string{
	// Shell: logo + section titles.
	"shell.logo.alt":   "LightCMS",
	"section.content":  "Content",
	"section.media":    "Media",
	"section.settings": "Settings",
	"section.tools":    "Tools",
	"section.inbox":    "Inbox",

	// Shell: sidebar nav items.
	"nav.dashboard":   "Dashboard",
	"nav.content":     "Content",
	"nav.templates":   "Templates",
	"nav.snippets":    "Snippets",
	"nav.collections": "Collections",
	"nav.folders":     "Folders",
	"nav.forks":       "Forks",
	"nav.imports":     "Imports",
	"nav.approvals":   "Approvals",
	"nav.assets":      "Asset Library",
	"nav.theme":       "Theme",
	"nav.redirects":   "Redirects",
	"nav.config":      "Configuration",
	"nav.apikeys":     "API Keys",
	"nav.security":    "Security",
	"nav.webhooks":    "Webhooks",
	"nav.users":       "Users",
	"nav.audit":       "Audit Log",
	"nav.analytics":   "Analytics",
	"nav.copilot":     "Copilot",
	"nav.agent":       "CMS Agent",
	"nav.search":      "End User Search",
	"nav.chat":        "Chat Widget",
	"nav.brokenlinks": "Broken Link Finder",
	"nav.messages":    "Messages",
	"nav.viewsite":    "View Site",
	"nav.logout":      "Logout",

	// Shell: language switch.
	"switch.label": "Language",
	"switch.zh":    "中文",
	"switch.en":    "EN",

	// Shell: delete / info / confirm / revert modals.
	"modal.delete.title":    "Confirm Delete",
	"modal.delete.body":     "Are you sure you want to delete this item?",
	"modal.delete.cancel":   "Cancel",
	"modal.delete.confirm":  "Delete",
	"modal.info.title":      "Information",
	"modal.info.ok":         "OK",
	"modal.confirm.title":   "Confirm",
	"modal.confirm.cancel":  "Cancel",
	"modal.confirm.confirm": "Confirm",
	"modal.revert.title":    "Confirm Revert",
	"modal.revert.cancel":   "Cancel",
	"modal.revert.confirm":  "Revert",

	// Shell: copilot drawer static text.
	"drawer.copilot":   "Copilot",
	"drawer.new":       "＋ New",
	"drawer.search.ph": "Search chats…",
	"drawer.input.ph":  "Ask the copilot…",
	"drawer.send":      "Send",

	// Login page.
	"login.title":       "Login",
	"login.subtitle":    "Content Management System",
	"login.email":       "Email",
	"login.email.ph":    "Enter email",
	"login.password":    "Password",
	"login.password.ph": "Enter password",
	"login.submit":      "Sign In",
	"login.logo.alt":    "LightCMS",
}
