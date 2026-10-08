package tui

// Wrapping canonical text is expensive compared to painting the visible rows.
// This cache belongs only to a terminal, expires with its bounded entry window,
// and is rebuilt on width/ASCII changes. Input, secrets and progress are excluded.
type bodyKey struct {
	role, text, peer, kind, code string
	clipped, unsafeLinks         bool
}
type cachedBody struct {
	rows []bodyLine
	seen uint64
}
type bodyCache struct {
	width   int
	ascii   bool
	frame   uint64
	entries map[bodyKey]cachedBody
}

func (c *bodyCache) begin(width int, ascii bool) {
	if c.width != width || c.ascii != ascii || c.entries == nil {
		c.entries = map[bodyKey]cachedBody{}
		c.width = width
		c.ascii = ascii
	}
	c.frame++
}
func (c *bodyCache) get(e Entry, width int, t Theme) []bodyLine {
	k := bodyKey{e.Role, e.Text, e.PeerID, e.PeerKind, e.Code, e.Clipped, e.UnsafeLinks}
	value, ok := c.entries[k]
	if !ok {
		value.rows = entryBody(e, width, t)
	}
	value.seen = c.frame
	c.entries[k] = value
	return value.rows
}
func (c *bodyCache) end() {
	for key, value := range c.entries {
		if value.seen != c.frame {
			delete(c.entries, key)
		}
	}
}
