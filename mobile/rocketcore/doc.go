// Package rocketcore is the rocket core for the Android app, built with
// gomobile bind into android/app/libs/rocketcore.aar (scripts/build-android.*;
// Java package io.github.donaldsteele.rocket.core.rocketcore). The app's
// Kotlin code does the UI and Android integration; everything on the wire
// (protocol v1: signing, verification, manifests, tar streaming, limits, path
// safety, HTTP send and receive, trust store, history) is rocket's Go code,
// the same as desktop rocket's.
//
// Only gomobile-safe types cross the boundary: string, bool, int, int64,
// []byte, error, the interfaces below and *Node. Lists and records cross as
// JSON in the same shapes desktop rocket uses (GET /api/peers, /api/history,
// /api/trusted, /api/incoming, /api/transfers, SSE events). File content
// crosses as file descriptors: Kotlin passes ParcelFileDescriptor.detachFd()
// and Go takes the descriptor over (os.NewFile) and closes it; a provider
// that has no descriptor returns -1 and the bytes go in chunks instead.
//
// # Node
//
//	NewNode(dataDir, name, version) (*Node, error)  identity (node.key), trust store,
//	                                                history and settings in dataDir
//	Name, SetName, Fingerprint, PublicKey, Version
//	AutoAccept, SetAutoAccept(bool) error           trusted senders skip the prompt (default on)
//	SetBeaconPort(port)                             UDP beacon port (default 29006, 0 = off)
//
// # Discovery
//
//	StartDiscovery(PeerListener) error  listen for beacons; OnPeers(json) on every change
//	StopDiscovery()
//	ObserveMDNS(name, fp, host, port, ver, os)  feed an NsdManager result in (merged by fp)
//	ForgetMDNS(fp)
//	PeersJSON() string                  [{name, fp, addr, os, version, via}]
//
// # Sending
//
//	Send(addr, fp, Source, ProgressListener) (transferID, error)
//	    addr is host:port, fp the expected fingerprint ("" to skip the check);
//	    with addr "" the peer is looked up by fp among discovered peers. The
//	    manifest is built and checked before Send returns; the transfer runs in
//	    the background and its events (progress, sent, error, cancelled) go to
//	    the ProgressListener only.
//	Cancel(transferID) bool             stop a send or receive, or decline a waiting offer
//	TransfersJSON() string              transfers in flight
//
// # Receiving
//
//	StartReceiver(port, Decider, Sink, EventListener) (boundPort, error)
//	    serve GET /api/hello, POST /api/offer and POST /api/recv on port (a
//	    free port when it is taken; 0 picks one) and announce on the beacon.
//	    Decider.Ask(offerJSON) shows an offer; answer with Answer. Every
//	    other event (offer_gone, progress, done, error, cancelled) goes to
//	    the EventListener.
//	StopReceiver()                      decline waiting offers, cancel receives, stop
//	Answer(offerID, accept, trust) bool trust pins the sender ("always accept")
//	IncomingJSON() string               offers waiting for a decision
//
// # History and trust
//
//	HistoryJSON(limit) (string, error), ClearHistory() error
//	TrustedJSON() (string, error), Revoke(fp) error
//
// Callbacks (listeners, Decider, Sink) are called on Go threads and must not
// block for long; Decider.Ask must return at once.
package rocketcore
