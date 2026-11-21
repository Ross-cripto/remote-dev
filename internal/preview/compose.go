package preview

type Compose struct {
	File     string `json:"file"`
	Override string `json:"override,omitempty"`
}

func (compose Compose) String() string {
	if compose.Override == "" {
		return compose.File
	}
	return compose.File + " + " + compose.Override
}
