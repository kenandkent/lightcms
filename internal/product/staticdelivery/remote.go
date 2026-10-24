package staticdelivery

import (
	"context"
	"errors"
)

type CompletionToken string

func (CompletionToken) MarshalJSON() ([]byte, error) {
	return nil, errors.New("completion token is private")
}

type Module struct {
	Name, MIME string
	Data       []byte
}
type UploadFile struct {
	PublicPath, MIME, SHA256 string
	Data                     []byte
}
type RemoteVersion struct{ ID, Marker string }
type RemoteDeployment struct{ ID, VersionID string }
type RemoteState struct {
	Exists                                          bool
	InstallationID, TargetID, ReleaseID             string
	VersionID, DeploymentID, SettingsHash, CodeHash string
	SingleVersion                                   bool
}
type Remote interface {
	Inspect(context.Context, Target) (RemoteState, error)
	Initialize(context.Context, Target, []Module) (RemoteVersion, error)
	UploadAssets(context.Context, Target, []UploadFile) (CompletionToken, error)
	UploadVersion(context.Context, Target, CompletionToken, []Module) (RemoteVersion, error)
	Activate(context.Context, Target, string) (RemoteDeployment, error)
}
