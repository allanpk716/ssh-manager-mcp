package cli

import "encoding/hex"

func hexDecode(s string) ([]byte, error) { return hex.DecodeString(s) }
func hexEncode(b []byte) string          { return hex.EncodeToString(b) }
