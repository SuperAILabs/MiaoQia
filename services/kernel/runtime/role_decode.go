package runtime

import (
	"encoding/json"
	"fmt"
	"go.yaml.in/yaml/v3"
)

// YAML不得把数值cell强制转成字符串，或把显式null scope声明当成缺省。
func (r *RoleSpec) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("角色必须是映射")
	}
	for i := 0; i < len(node.Content); i += 2 {
		name, value := node.Content[i].Value, node.Content[i+1]
		for value.Kind == yaml.AliasNode {
			value = value.Alias
		}
		switch name {
		case "cell":
			if value.Tag != "!!str" {
				return fmt.Errorf("cell必须是字符串")
			}
		case "data_scopes":
			if value.Kind != yaml.MappingNode {
				return fmt.Errorf("data_scopes必须是映射")
			}
			for _, child := range value.Content {
				for child.Kind == yaml.AliasNode {
					child = child.Alias
				}
				if child.Tag != "!!str" {
					return fmt.Errorf("data_scopes的键和值必须是字符串")
				}
			}
		case "subsystems":
			if value.Kind != yaml.SequenceNode {
				return fmt.Errorf("subsystems必须是列表")
			}
			for _, child := range value.Content {
				for child.Kind == yaml.AliasNode {
					child = child.Alias
				}
				if child.Tag != "!!str" {
					return fmt.Errorf("子系统名称必须是字符串")
				}
			}
		}
	}
	type plain RoleSpec
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*r = RoleSpec(decoded)
	return nil
}

func (r *RoleSpec) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range []string{"cell", "data_scopes"} {
		if value, exists := fields[name]; exists && string(value) == "null" {
			return fmt.Errorf("%s不能为null", name)
		}
	}
	type plain RoleSpec
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = RoleSpec(decoded)
	return nil
}
