package server

import (
	"github.com/go-playground/validator/v10"
	"github.com/tadjov/tusker/internal/apperr"
)

type requestValidator struct {
	validate *validator.Validate
}

func newRequestValidator() *requestValidator {
	return &requestValidator{validate: validator.New(validator.WithRequiredStructEnabled())}
}

func (v *requestValidator) Validate(i any) error {
	if err := v.validate.Struct(i); err != nil {
		return apperr.NewValidationWrap("request validation failed", err)
	}
	return nil
}

func (s *Server) SetupValidator() *Server {
	s.Echo.Validator = newRequestValidator()

	return s
}
