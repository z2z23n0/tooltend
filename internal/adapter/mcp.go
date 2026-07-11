package adapter

import (
	"context"
	"errors"
)

type RemoteMCP struct{}

func (RemoteMCP) Name() string        { return "remote-http-mcp" }
func (RemoteMCP) Kinds() []SourceKind { return []SourceKind{SourceHTTP} }
func (RemoteMCP) Capabilities() Capabilities {
	// Remote MCP discovery is an availability/config observation, not a
	// version resolver. Advertising Check would make the generic worker call
	// Resolve and manufacture a failed update notification on every interval.
	return Capabilities{RemoteOnly: true}
}

func (RemoteMCP) Normalize(source Source) (Source, error) {
	return CanonicalizeSource(SourceHTTP, source)
}

func (RemoteMCP) Resolve(context.Context, Source, Track) (Resolved, error) {
	return Resolved{}, errors.New("remote HTTP MCP versions are observed but not managed")
}
func (RemoteMCP) Fetch(context.Context, Source, Resolved, string) (Artifact, error) {
	return Artifact{}, errors.New("remote HTTP MCP versions are observed but not managed")
}
func (RemoteMCP) Verify(context.Context, Source, Resolved, Artifact) error {
	return errors.New("remote HTTP MCP versions are observed but not managed")
}
