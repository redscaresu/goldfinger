package models

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRepoFullName(t *testing.T) {
	r := Repo{Owner: "redscaresu", Name: "goldfinger"}
	assert.Equal(t, "redscaresu/goldfinger", r.FullName())
}
