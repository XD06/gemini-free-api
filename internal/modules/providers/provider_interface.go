package providers

import "context"

// Provider defines the interface that all AI providers must implement
type Provider interface {
	// Init initializes the provider with authentication
	Init(ctx context.Context) error

	// GenerateContent generates a single response
	GenerateContent(ctx context.Context, prompt string, options ...GenerateOption) (*Response, error)

	// StartChat creates a new chat session
	StartChat(options ...ChatOption) ChatSession

	// Close cleans up resources
	Close() error

	// GetName returns the provider name
	GetName() string

	// IsHealthy checks if the provider is ready to serve requests
	IsHealthy() bool

	// ListModels returns models supported by this provider
	ListModels() []ModelInfo
}

// ChatSession represents a multi-turn conversation
type ChatSession interface {
	// SendMessage sends a message and returns the response
	SendMessage(ctx context.Context, message string, options ...GenerateOption) (*Response, error)

	// GetMetadata returns session metadata for persistence
	GetMetadata() *SessionMetadata

	// GetHistory returns the conversation history
	GetHistory() []Message

	// Clear clears the conversation history
	Clear()
}

// Response represents a provider's response
type Response struct {
	Text           string         `json:"text"`
	Images         []Image        `json:"images,omitempty"`
	Candidates     []Candidate    `json:"candidates,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	ChosenIndex    int            `json:"chosen_index"`
	ConversationID string         `json:"conversation_id,omitempty"`
	ResponseID     string         `json:"response_id,omitempty"`
}

// Message represents a single message in conversation
type Message struct {
	Role    string  `json:"role"` // "user" or "model"
	Content string  `json:"content"`
	Images  []Image `json:"images,omitempty"`
}

// Image represents an image in the response
type Image struct {
	URL      string `json:"url"`
	Title    string `json:"title,omitempty"`
	AltText  string `json:"alt_text,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

// Candidate represents an alternative response
type Candidate struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

// SessionMetadata contains information to restore a session
type SessionMetadata struct {
	ConversationID string         `json:"conversation_id"`
	ResponseID     string         `json:"response_id"`
	ChoiceID       string         `json:"choice_id"`
	Model          string         `json:"model,omitempty"`
	Extra          map[string]any `json:"extra,omitempty"`
}

// GenerateOption configures generation behavior
type GenerateOption func(*GenerateConfig)

// GenerateConfig holds generation configuration
type GenerateConfig struct {
	Model          string
	Files          []string
	InputFiles     []InputFile
	Temperature    float64
	MaxTokens      int
	ThinkingLevel  string
	ConversationID string
	SourcePath     bool
}

// InputFile is an in-memory file to upload with a generation request.
type InputFile struct {
	Name     string
	MimeType string
	Data     []byte
}

// ChatOption configures chat session behavior
type ChatOption func(*ChatConfig)

// ChatConfig holds chat session configuration
type ChatConfig struct {
	Model    string
	Metadata *SessionMetadata
}

// WithModel sets the model to use
func WithModel(model string) GenerateOption {
	return func(c *GenerateConfig) {
		c.Model = model
	}
}

// WithFiles adds files to the request
func WithFiles(files []string) GenerateOption {
	return func(c *GenerateConfig) {
		c.Files = files
	}
}

// WithInputFiles adds in-memory files to the request.
func WithInputFiles(files []InputFile) GenerateOption {
	return func(c *GenerateConfig) {
		c.InputFiles = files
	}
}

// WithThinkingLevel sets the Gemini web thinking level. Supported values are
// "standard" and "extended"; unknown values are ignored by the provider.
func WithThinkingLevel(level string) GenerateOption {
	return func(c *GenerateConfig) {
		c.ThinkingLevel = level
	}
}

// WithConversationID enables server-side Gemini web conversation context for
// requests that share the same client-provided conversation ID.
func WithConversationID(id string) GenerateOption {
	return func(c *GenerateConfig) {
		c.ConversationID = id
	}
}

// WithSourcePath controls whether Gemini Web continuation requests include the
// URL source-path=/app/<conversation> parameter. Conversation metadata is still
// sent in the request body when this is false.
func WithSourcePath(enabled bool) GenerateOption {
	return func(c *GenerateConfig) {
		c.SourcePath = enabled
	}
}

// WithChatModel sets the model for chat session
func WithChatModel(model string) ChatOption {
	return func(c *ChatConfig) {
		c.Model = model
	}
}

// WithChatMetadata restores a previous chat session
func WithChatMetadata(metadata *SessionMetadata) ChatOption {
	return func(c *ChatConfig) {
		c.Metadata = metadata
	}
}
