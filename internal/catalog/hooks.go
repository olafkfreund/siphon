package catalog

import (
	"slices"
	"strings"
)

// awsServers maps the form's server choice to its package and source suffix.
var awsServers = []struct{ Key, Package string }{
	{"cloudwatch", "aws-cloudwatch"},
	{"docs", "aws-docs"},
}

// awsHook is the AWS-specific checking the templates can't express: the
// chosen servers must be installed, and role/profile mode need their parts.
func awsHook(c *Ctx) error {
	chosen := splitList(c.Values["servers"])
	var srcs []string
	for _, sv := range awsServers {
		on := slices.Contains(chosen, sv.Key)
		c.Data[sv.Key] = on
		if !on {
			continue
		}
		if _, ok := c.Cfg.Server.MCPPackages[sv.Package]; !ok {
			return bad("the %s server isn't installed: enable services.siphon.aws in your NixOS config", sv.Key)
		}
		srcs = append(srcs, "mcp__"+c.Values["name"]+"-"+sv.Key)
	}
	if len(srcs) == 0 {
		return bad("choose at least one server")
	}
	c.Done.ReadTools = "[" + strings.Join(srcs, ", ") + "]"
	v := c.Values
	if v["mode"] == "role" {
		if v["role_arn"] == "" {
			return bad("a role ARN is required")
		}
		if (v["access_key_id"] == "") != (v["secret_access_key"] == "") {
			return bad("base access keys go together: give both or neither")
		}
	} else if v["profile"] == "" {
		return bad("a profile name is required")
	}
	return nil
}
