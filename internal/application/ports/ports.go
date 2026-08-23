// Package ports declares what the application layer needs from the outside
// world, in the application's own vocabulary.
//
// Every interface here is declared by the side that *calls* it, not the side
// that implements it. That is the whole point: infrastructure depends on this
// package, never the reverse, so the direction of the dependency and the
// direction of the call are opposites. An adapter can be swapped, faked or
// deleted without the orchestration noticing.
//
// The request and result types live here for the same reason. A port whose
// types belong to the adapter is not a port — anything holding one still has
// to import the adapter to say what it is passing.
//
// Names are prefixed by the port they serve (ProveRequest, PublishRequest)
// because this package is flat and Request alone would collide three ways.
// Unlike RunArtifact and PipelineArtifact, these are names people say.
package ports
