/*
Merlin is a post-exploitation command and control framework.

This file is part of Merlin.
Copyright (C) 2023 Russel Van Tuyl

Merlin is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
any later version.

Merlin is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with Merlin.  If not, see <http://www.gnu.org/licenses/>.
*/

// Package opaque is an authenticator for Agent communications with the server using the OPAQUE protocol
package opaque

import (

	// Standard
	"crypto/sha256"
	"fmt"

	// 3rd Party
	"github.com/cretz/gopaque/gopaque"
	"github.com/google/uuid"
	"golang.org/x/crypto/pbkdf2"

	// Merlin
	"github.com/Ne0nd0g/merlin-message"
	"github.com/Ne0nd0g/merlin-message/opaque"

	// Internal
	"github.com/Ne0nd0g/merlin-agent/v2/cli"
	"github.com/Ne0nd0g/merlin-agent/v2/core"
)

// Authenticator is a structure used for OPAQUE authentication
type Authenticator struct {
	agent         uuid.UUID // The Agent's ID
	registered    bool      // If OPAQUE registration has been completed
	authenticated bool      // If OPAQUE authentication has been completed
	opaque        *User     // The OPAQUE user data structure
}

// New returns an OPAQUE Authenticator structure used for Agent authentication
func New(id uuid.UUID) *Authenticator {
	return &Authenticator{agent: id}
}

// Authenticate goes through the entire OPAQUE process to authenticate to the server and establish a shared secret
func (a *Authenticator) Authenticate(in messages.Base) (out messages.Base, authenticated bool, err error) {
	out.ID = a.agent
	out.Type = messages.OPAQUE

	// Check for ReRegister and ReAuthenticate messages
	if in.Type == messages.OPAQUE {
		if in.Payload != nil {
			switch in.Payload.(opaque.Opaque).Type {
			case opaque.ReRegister:
				cli.Message(cli.NOTE, "Received OPAQUE re-register request")
				if !a.registered {
					cli.Message(cli.INFO, "authenticators/opaque.Authenticate(): OPAQUE registration already in progress, doing nothing")
					return messages.Base{}, false, nil
				}
				a.registered = false
				a.opaque = nil
			case opaque.ReAuthenticate:
				cli.Message(cli.NOTE, "Received OPAQUE re-authenticate request")
				a.authenticated = false
				payload := opaque.Opaque{
					Type:    opaque.RegComplete,
					Payload: nil,
				}
				in.Payload = payload
			}
		}
	}

	// Registration has not successfully completed
	if !a.registered {
		// The initial OPAQUE message generated by the Agent for its first communication with the server will have an empty payload.
		// All other messages will have an opaque.Opaque payload
		if in.Payload == nil {
			// Register Init
			out.Payload, a.opaque, err = UserRegisterInit(a.agent, a.opaque)
			if err != nil {
				err = fmt.Errorf("authenticators/opaque.Authenticate(): there was an error creating the OPAQUE User Registration Initialization message: %s", err)
			}
			// Return opaque.RegInit message
			cli.Message(cli.NOTE, "Starting OPAQUE Registration")
			return
		}
	}

	// Validate the incoming message is for this agent
	if in.ID != a.agent {
		return messages.Base{}, false, fmt.Errorf("authenticators/opaque.Authenticate(): Incoming message ID %s does not match Agent ID %s", in.ID, a.agent)
	}

	// Validate the Base message is an OPAQUE type
	if in.Type != messages.OPAQUE {
		return out, authenticated, fmt.Errorf("authenticators/opaque.Authenticate(): Incoming message type %d was not an OPAQUE type %d", in.Type, messages.OPAQUE)
	}

	// AuthComplete messages have no payload
	opaqueMessage := in.Payload.(opaque.Opaque)

	switch opaqueMessage.Type {
	case opaque.RegInit:
		// Server returned a RegInit message, start OPAQUE registration completion
		out.Payload, err = UserRegisterComplete(opaqueMessage, a.opaque)
		if err != nil {
			err = fmt.Errorf("authenticators/opaque.Authenticate(): there was an error creating the OPAQUE User Registration Complete message: %s", err)
		} else {
			a.registered = true
		}
		// Returning an opaque.RegComplete message to the server
	case opaque.RegComplete:
		cli.Message(cli.NOTE, "Received OPAQUE server registration complete message")
		cli.Message(cli.NOTE, "Starting OPAQUE Authentication")
		// OPAQUE Registration has completed, start OPAQUE Authentication
		// Build AuthInit message
		out.Payload, err = UserAuthenticateInit(a.agent, a.opaque)
		// Returning an opaque.AuthInit message to the server
	case opaque.AuthInit:
		cli.Message(cli.NOTE, "Received OPAQUE server authentication initialization message")
		// Server returned an AuthInit message, start authentication completion
		out.Payload, err = UserAuthenticateComplete(opaqueMessage, a.opaque)
		if err == nil {
			a.authenticated = true
			authenticated = true
		}
		// Returning an opaque.AuthComplete message to the server
	case opaque.ReRegister:
		cli.Message(cli.NOTE, "Received OPAQUE server re-registration message")
		a.registered = false
		a.opaque = nil
		out.Payload, a.opaque, err = UserRegisterInit(a.agent, a.opaque)
	case opaque.ReAuthenticate:
		cli.Message(cli.NOTE, "Received OPAQUE server re-authentication message")
		a.authenticated = false
		out.Payload, err = UserAuthenticateInit(a.agent, a.opaque)
		// Returning an opaque.AuthInit message to the server
	}
	return
}

// Secret returns the established shared secret as bytes
func (a *Authenticator) Secret() (key []byte, err error) {
	if !a.authenticated {
		return nil, fmt.Errorf("authenticators/opaque.Secret(): the Agent has not completed OPAQUE authentication")
	}
	return []byte(a.opaque.Kex.SharedSecret.String()), nil
}

// String returns the name of the Authenticator type
func (a *Authenticator) String() string {
	return "OPAQUE"
}

// User is the structure that holds information for the various steps of the OPAQUE protocol as the user
type User struct {
	reg         *gopaque.UserRegister         // User Registration
	regComplete *gopaque.UserRegisterComplete // User Registration Complete
	auth        *gopaque.UserAuth             // User Authentication
	Kex         *gopaque.KeyExchangeSigma     // User Key Exchange
	pwdU        []byte                        // User Password
}

// UserRegisterInit is used to perform the OPAQUE Password Authenticated Key Exchange (PAKE) protocol Registration steps for the user
func UserRegisterInit(AgentID uuid.UUID, user *User) (opaque.Opaque, *User, error) {
	cli.Message(cli.DEBUG, "Entering into opaque.UserRegisterInit...")
	var userRegInit *gopaque.UserRegisterInit
	// If Registration was previously started, but unsuccessful, the User variable will not be nil
	if user == nil {
		var newUser User
		// Generate a random password and run it through 5000 iterations of PBKDF2; Used with OPAQUE
		x := core.RandStringBytesMaskImprSrc(30)

		agentIDBytes, err := AgentID.MarshalBinary()
		if err != nil {
			return opaque.Opaque{}, nil, fmt.Errorf("there was an error marshalling the AgentID to bytes: %s", err)
		}

		newUser.pwdU = pbkdf2.Key([]byte(x), agentIDBytes, 5000, 32, sha256.New)

		// Build OPAQUE User Registration Initialization
		newUser.reg = gopaque.NewUserRegister(gopaque.CryptoDefault, agentIDBytes, nil)
		user = &newUser
	}

	userRegInit = user.reg.Init(user.pwdU)
	cli.Message(cli.DEBUG, fmt.Sprintf("OPAQUE UserID: %x", userRegInit.UserID))
	cli.Message(cli.DEBUG, fmt.Sprintf("OPAQUE Alpha: %v", userRegInit.Alpha))
	cli.Message(cli.DEBUG, fmt.Sprintf("OPAQUE PwdU: %x", user.pwdU))

	userRegInitBytes, errUserRegInitBytes := userRegInit.ToBytes()
	if errUserRegInitBytes != nil {
		return opaque.Opaque{}, user, fmt.Errorf("there was an error marshalling the OPAQUE user registration initialization message to bytes:\r\n%s", errUserRegInitBytes.Error())
	}

	// Message to be sent to the server
	regInit := opaque.Opaque{
		Type:    opaque.RegInit,
		Payload: userRegInitBytes,
	}

	return regInit, user, nil
}

// UserRegisterComplete consumes the Server's response and finishes OPAQUE registration
func UserRegisterComplete(regInitResp opaque.Opaque, user *User) (opaque.Opaque, error) {
	cli.Message(cli.DEBUG, "Entering into opaque.UserRegisterComplete...")

	if regInitResp.Type != opaque.RegInit {
		return opaque.Opaque{}, fmt.Errorf("expected OPAQUE message type %d, got %d", opaque.RegInit, regInitResp.Type)
	}

	// Check to see if OPAQUE User Registration was previously completed
	if user.regComplete == nil {
		var serverRegInit gopaque.ServerRegisterInit

		errServerRegInit := serverRegInit.FromBytes(gopaque.CryptoDefault, regInitResp.Payload)
		if errServerRegInit != nil {
			return opaque.Opaque{}, fmt.Errorf("there was an error unmarshalling the OPAQUE server register initialization message from bytes:\r\n%s", errServerRegInit.Error())
		}

		cli.Message(cli.NOTE, "Received OPAQUE server registration initialization message")
		cli.Message(cli.DEBUG, fmt.Sprintf("OPAQUE Beta: %v", serverRegInit.Beta))
		cli.Message(cli.DEBUG, fmt.Sprintf("OPAQUE V: %v", serverRegInit.V))
		cli.Message(cli.DEBUG, fmt.Sprintf("OPAQUE PubS: %s", serverRegInit.ServerPublicKey))

		// TODO extend gopaque to run RwdU through n iterations of PBKDF2
		user.regComplete = user.reg.Complete(&serverRegInit)
	}

	userRegCompleteBytes, errUserRegCompleteBytes := user.regComplete.ToBytes()
	if errUserRegCompleteBytes != nil {
		return opaque.Opaque{}, fmt.Errorf("there was an error marshalling the OPAQUE user registration complete message to bytes:\r\n%s", errUserRegCompleteBytes.Error())
	}

	cli.Message(cli.DEBUG, fmt.Sprintf("OPAQUE EnvU: %x", user.regComplete.EnvU))
	cli.Message(cli.DEBUG, fmt.Sprintf("OPAQUE PubU: %v", user.regComplete.UserPublicKey))

	// message to be sent to the server
	regComplete := opaque.Opaque{
		Type:    opaque.RegComplete,
		Payload: userRegCompleteBytes,
	}

	return regComplete, nil
}

// UserAuthenticateInit is used to authenticate an agent leveraging the OPAQUE Password Authenticated Key Exchange (PAKE) protocol
func UserAuthenticateInit(AgentID uuid.UUID, user *User) (opaque.Opaque, error) {
	cli.Message(cli.DEBUG, "Entering into opaque.UserAuthenticateInit...")

	agentIDBytes, err := AgentID.MarshalBinary()
	if err != nil {
		return opaque.Opaque{}, fmt.Errorf("there was an error marshalling the AgentID to bytes: %s", err)
	}

	// 1 - Create a NewUserAuth with an embedded key exchange
	user.Kex = gopaque.NewKeyExchangeSigma(gopaque.CryptoDefault)
	user.auth = gopaque.NewUserAuth(gopaque.CryptoDefault, agentIDBytes, user.Kex)

	// 2 - Call Init with the password and send the resulting UserAuthInit to the server
	userAuthInit, err := user.auth.Init(user.pwdU)
	if err != nil {
		return opaque.Opaque{}, fmt.Errorf("there was an error creating the OPAQUE user authentication initialization message:\r\n%s", err.Error())
	}

	userAuthInitBytes, errUserAuthInitBytes := userAuthInit.ToBytes()
	if errUserAuthInitBytes != nil {
		return opaque.Opaque{}, fmt.Errorf("there was an error marshalling the OPAQUE user authentication initialization message to bytes:\r\n%s", errUserAuthInitBytes.Error())
	}

	// message to be sent to the server
	authInit := opaque.Opaque{
		Type:    opaque.AuthInit,
		Payload: userAuthInitBytes,
	}

	return authInit, nil
}

// UserAuthenticateComplete consumes the Server's authentication message and finishes the user authentication and key exchange
func UserAuthenticateComplete(authInitResp opaque.Opaque, user *User) (opaque.Opaque, error) {
	cli.Message(cli.DEBUG, "Entering into opaque.UserAuthenticateComplete...")

	if authInitResp.Type != opaque.AuthInit {
		return opaque.Opaque{}, fmt.Errorf("expected OPAQUE message type: %d, received: %d", opaque.AuthInit, authInitResp.Type)
	}

	// 3 - Receive the server's ServerAuthComplete
	var serverComplete gopaque.ServerAuthComplete

	errServerComplete := serverComplete.FromBytes(gopaque.CryptoDefault, authInitResp.Payload)
	if errServerComplete != nil {
		return opaque.Opaque{}, fmt.Errorf("there was an error unmarshalling the OPAQUE server complete message from bytes:\r\n%s", errServerComplete.Error())
	}

	// 4 - Call Complete with the server's ServerAuthComplete. The resulting UserAuthFinish has user and server key
	// information. This would be the last step if we were not using an embedded key exchange. Since we are, take the
	// resulting UserAuthComplete and send it to the server.
	cli.Message(cli.NOTE, "Received OPAQUE server complete message")
	cli.Message(cli.DEBUG, fmt.Sprintf("OPAQUE Beta: %x", serverComplete.Beta))
	cli.Message(cli.DEBUG, fmt.Sprintf("OPAQUE V: %x", serverComplete.V))
	cli.Message(cli.DEBUG, fmt.Sprintf("OPAQUE PubS: %x", serverComplete.ServerPublicKey))
	cli.Message(cli.DEBUG, fmt.Sprintf("OPAQUE EnvU: %x", serverComplete.EnvU))

	_, userAuthComplete, errUserAuth := user.auth.Complete(&serverComplete)
	if errUserAuth != nil {
		return opaque.Opaque{}, fmt.Errorf("there was an error completing OPAQUE authentication:\r\n%s", errUserAuth)
	}

	userAuthCompleteBytes, errUserAuthCompleteBytes := userAuthComplete.ToBytes()
	if errUserAuthCompleteBytes != nil {
		return opaque.Opaque{}, fmt.Errorf("there was an error marshalling the OPAQUE user authentication complete message to bytes:\r\n%s", errUserAuthCompleteBytes.Error())
	}

	authComplete := opaque.Opaque{
		Type:    opaque.AuthComplete,
		Payload: userAuthCompleteBytes,
	}

	return authComplete, nil
}
