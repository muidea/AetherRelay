package biz

import codexidentity "aetherrelay/internal/pkg/aetherrelaycodexidentity"

// currentIdentity is a block-local value copy of the shared credential and
// inference identity authority. Contract: CP-CLIENT-002..003, CP-HDR-003..006.
var currentIdentity = codexidentity.Current()
