package domain

import "time"

// Bounds describes a node rectangle in screen pixels.
type Bounds struct {
	Left   int `json:"left"`
	Top    int `json:"top"`
	Right  int `json:"right"`
	Bottom int `json:"bottom"`
}

// ScreenNode is one observed accessibility-tree node.
type ScreenNode struct {
	ID        string   `json:"id"`
	Text      string   `json:"text,omitempty"`
	Bounds    Bounds   `json:"bounds"`
	Enabled   bool     `json:"enabled"`
	Visible   bool     `json:"visible"`
	Actions   []string `json:"actions,omitempty"`
	Sensitive bool     `json:"sensitive,omitempty"`
}

// ScreenSnapshot is one versioned observation of the foreground screen.
type ScreenSnapshot struct {
	Package   string       `json:"package"`
	Version   string       `json:"version"`
	Width     int          `json:"width"`
	Height    int          `json:"height"`
	Protected bool         `json:"protected,omitempty"`
	Sensitive bool         `json:"sensitive,omitempty"`
	Nodes     []ScreenNode `json:"nodes"`
}

// ImageInput is an explicitly consented image bound to one screen version.
type ImageInput struct {
	MediaType     string `json:"media_type"`
	DataBase64    string `json:"data_base64"`
	Consent       bool   `json:"consent"`
	ScreenVersion string `json:"screen_version"`
}

// TurnRequest contains one user utterance and its screen observation.
type TurnRequest struct {
	RequestID string         `json:"request_id"`
	Text      string         `json:"text"`
	Screen    ScreenSnapshot `json:"screen"`
	Image     *ImageInput    `json:"image,omitempty"`
}

// ReplyKind identifies one allowlisted assistant reply shape.
type ReplyKind string

// Allowlisted assistant reply kinds.
const (
	ReplyExplain        ReplyKind = "explain"
	ReplyHighlight      ReplyKind = "highlight"
	ReplyClarify        ReplyKind = "clarify"
	ReplyActionProposal ReplyKind = "action_proposal"
	ReplyNeedImage      ReplyKind = "need_image"
	ReplyRefuse         ReplyKind = "refuse"
)

// ActionKind identifies one allowlisted client action intent.
type ActionKind string

// Allowlisted action kinds.
const (
	ActionOpenApp       ActionKind = "OPEN_APP"
	ActionPrepareDialer ActionKind = "PREPARE_DIALER"
)

// ActionProposal is a server-bound action awaiting explicit confirmation.
type ActionProposal struct {
	ActionID    string     `json:"action_id"`
	Kind        ActionKind `json:"kind"`
	AppRef      string     `json:"app_ref,omitempty"`
	AppLabel    string     `json:"app_label,omitempty"`
	ContactRef  string     `json:"contact_ref,omitempty"`
	DisplayText string     `json:"display_text"`
}

// Reply is the validated, server-stamped response for one turn.
type Reply struct {
	Kind             ReplyKind       `json:"kind"`
	Text             string          `json:"text"`
	HighlightNodeIDs []string        `json:"highlight_node_ids,omitempty"`
	Action           *ActionProposal `json:"action,omitempty"`
	Generation       uint64          `json:"generation"`
	ScreenVersion    string          `json:"screen_version"`
}

// App is a user-configured local application reference.
type App struct {
	Ref     string   `json:"ref"`
	Label   string   `json:"label"`
	Aliases []string `json:"aliases,omitempty"`
}

// Contact is a user-configured local favourite reference.
type Contact struct {
	Ref     string   `json:"ref"`
	Label   string   `json:"label"`
	Aliases []string `json:"aliases,omitempty"`
}

// SessionConfig contains device-bound opaque local targets.
type SessionConfig struct {
	DeviceID string
	Apps     []App     `json:"apps"`
	Contacts []Contact `json:"contacts,omitempty"`
}

// Session describes a volatile backend session.
type Session struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// TranscriptionRequest contains one bounded WAV transcription operation.
type TranscriptionRequest struct {
	RequestID string `json:"request_id"`
	WAV       []byte `json:"-"`
}

// Transcription is bounded text returned by the transcription provider.
type Transcription struct {
	Text string `json:"text"`
}

// ConfirmationDecision records an explicit user decision.
type ConfirmationDecision string

// Allowlisted confirmation decisions.
const (
	DecisionConfirm ConfirmationDecision = "CONFIRM"
	DecisionDecline ConfirmationDecision = "DECLINE"
)

// ConfirmationRequest binds a decision to one issued proposal.
type ConfirmationRequest struct {
	RequestID     string               `json:"request_id"`
	ActionID      string               `json:"action_id"`
	ScreenVersion string               `json:"screen_version"`
	Generation    uint64               `json:"generation"`
	Decision      ConfirmationDecision `json:"decision"`
}

// ActionGrant authorizes one allowlisted client action for a short interval.
type ActionGrant struct {
	ActionID   string     `json:"action_id"`
	Kind       ActionKind `json:"kind"`
	AppRef     string     `json:"app_ref,omitempty"`
	AppLabel   string     `json:"app_label,omitempty"`
	ContactRef string     `json:"contact_ref,omitempty"`
	ExpiresAt  time.Time  `json:"expires_at"`
}

// ConfirmationResult reports whether a proposal was confirmed or declined.
type ConfirmationResult struct {
	Status string       `json:"status"`
	Grant  *ActionGrant `json:"grant,omitempty"`
}

// ActionResultRequest reports the observed result of an issued grant.
type ActionResultRequest struct {
	RequestID string `json:"request_id"`
	ActionID  string `json:"action_id"`
	Status    string `json:"status"`
}

// CancelRequest terminates a session idempotently.
type CancelRequest struct {
	RequestID string `json:"request_id"`
}

// PlanInput is the sanitized input passed to a planner.
type PlanInput struct {
	Text     string
	Screen   ScreenSnapshot
	Image    *ImageInput
	Apps     []App
	Contacts []Contact
}

// PlanOutput is the untrusted structured reply returned by a planner.
type PlanOutput struct{ Reply Reply }

// Device is the authenticated opaque device identity.
type Device struct{ ID string }
