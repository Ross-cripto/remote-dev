package panel

type key byte

const keyInterrupt key = 3

const (
	keyUp key = 0x80 + iota
	keyDown
	keyPageUp
	keyPageDown
)

type keyDecoder struct {
	escaped    bool
	introducer byte
	parameter  byte
}

func (decoder *keyDecoder) feed(input []byte) []key {
	var keys []key
	for _, character := range input {
		if character == byte(keyInterrupt) {
			*decoder = keyDecoder{}
			keys = append(keys, keyInterrupt)
			continue
		}
		if character == '\x1b' {
			*decoder = keyDecoder{escaped: true}
			continue
		}
		if decoder.escaped {
			if decoded := decoder.escapeKey(character); decoded != 0 {
				keys = append(keys, decoded)
			}
			continue
		}
		if character >= ' ' && character <= '~' {
			keys = append(keys, key(character))
		}
	}
	return keys
}

func (decoder *keyDecoder) escapeKey(character byte) key {
	if decoder.introducer == 0 {
		if character == '[' || character == 'O' {
			decoder.introducer = character
			return 0
		}
		*decoder = keyDecoder{}
		return 0
	}
	if character >= '0' && character <= '?' {
		if decoder.parameter == 0 {
			decoder.parameter = character
		}
		return 0
	}

	parameter := decoder.parameter
	*decoder = keyDecoder{}
	switch character {
	case 'A':
		return keyUp
	case 'B':
		return keyDown
	case '~':
		switch parameter {
		case '5':
			return keyPageUp
		case '6':
			return keyPageDown
		}
	}
	return 0
}
