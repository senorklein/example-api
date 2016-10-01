package accounts

import (
	"errors"
	"fmt"
	"time"

	"github.com/RichardKnop/example-api/oauth"
	"github.com/RichardKnop/example-api/oauth/roles"
	"github.com/jinzhu/gorm"
)

var (
	// ErrInvitationNotFound ...
	ErrInvitationNotFound = errors.New("Invitation not found")
)

// FindInvitationByReference looks up an invitation by a reference and returns it
// only return the object if it's not expired
func (s *Service) FindInvitationByReference(reference string) (*Invitation, error) {
	// Fetch the invitation from the database
	invitation := new(Invitation)
	notFound := InvitationPreload(s.db).Where("reference = ?", reference).
		Where("expires_at > ?", time.Now().UTC()).First(invitation).RecordNotFound()

	// Not found
	if notFound {
		return nil, ErrInvitationNotFound
	}

	return invitation, nil
}

// InviteUser invites a new user and sends an invitation email
func (s *Service) InviteUser(invitedByUser *User, invitationRequest *InvitationRequest) (*Invitation, error) {
	// Check if oauth user exists
	if s.GetOauthService().UserExists(invitationRequest.Email) {
		return nil, oauth.ErrUsernameTaken
	}

	// Begin a transaction
	tx := s.db.Begin()

	// Create a new oauth user without a password
	oauthUser, err := s.GetOauthService().CreateUserTx(
		tx,
		roles.User,
		invitationRequest.Email,
		"", // password
	)
	if err != nil {
		return nil, err
	}

	// Create a new user account
	invitedUser, err := NewUser(
		invitedByUser.Account,
		oauthUser,
		"",    // facebook ID
		false, // confirmed
		&UserRequest{
			FirstName: invitationRequest.FirstName,
			LastName:  invitationRequest.LastName,
		},
	)
	if err != nil {
		return nil, err
	}

	// Save the user to the database
	if err = tx.Create(invitedUser).Error; err != nil {
		return nil, err
	}

	// Assign related objects
	invitedUser.Account = invitedByUser.Account
	invitedUser.OauthUser = oauthUser

	// Update the meta user ID field
	err = tx.Model(oauthUser).UpdateColumn(oauth.User{MetaUserID: invitedUser.ID}).Error
	if err != nil {
		return nil, err
	}

	// Create a new invitation
	invitation, err := NewInvitation(invitedUser, invitedByUser, s.cnf.AppSpecific.InvitationLifetime)
	if err != nil {
		return nil, err
	}

	// Save the invitation to the database
	if err := tx.Create(invitation).Error; err != nil {
		return nil, err
	}

	// Assign related objects
	invitation.InvitedUser = invitedUser
	invitation.InvitedByUser = invitedByUser

	// Commit the transaction
	if err := tx.Commit().Error; err != nil {
		tx.Rollback() // rollback the transaction
		return nil, err
	}

	// Send invitation email
	go func() {
		if err := s.sendInvitationEmail(invitation); err != nil {
			logger.Error(invitation)
		}
	}()

	return invitation, nil
}

// ConfirmInvitation sets password on the oauth user object and deletes the invitation
func (s *Service) ConfirmInvitation(invitation *Invitation, password string) error {
	// Begin a transaction
	tx := s.db.Begin()

	// Set the new password
	err := s.oauthService.SetPasswordTx(
		tx,
		invitation.InvitedUser.OauthUser,
		password,
	)
	if err != nil {
		tx.Rollback() // rollback the transaction
		return err
	}

	// Soft delete the invitation
	if err := tx.Delete(invitation).Error; err != nil {
		tx.Rollback() // rollback the transaction
		return err
	}

	// Commit the transaction
	if err := tx.Commit().Error; err != nil {
		tx.Rollback() // rollback the transaction
		return err
	}

	return nil
}

func (s *Service) sendInvitationEmail(invitation *Invitation) error {
	invitationEmail, err := s.emailFactory.NewInvitationEmail(invitation)
	if err != nil {
		return fmt.Errorf("New invitation email error: %s", err)
	}

	// Try to send the invitation email
	if err := s.emailService.Send(invitationEmail); err != nil {
		return fmt.Errorf("Send email error: %s", err)
	}

	// If the email was sent successfully, update the email_sent flag
	now := gorm.NowFunc()
	return s.db.Model(invitation).UpdateColumns(Invitation{
		EmailTokenModel: EmailTokenModel{
			EmailSent:   true,
			EmailSentAt: &now,
			Model:       gorm.Model{UpdatedAt: time.Now().UTC()},
		},
	}).Error
}
