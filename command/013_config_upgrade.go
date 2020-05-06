package command

import (
	"fmt"
	"io/ioutil"
	"os"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/hashicorp/terraform/addrs"
	"github.com/hashicorp/terraform/configs"
	"github.com/hashicorp/terraform/internal/getproviders"
	"github.com/hashicorp/terraform/tfdiags"
	"github.com/zclconf/go-cty/cty"
)

// ZeroThirteenUpgradeCommand upgrades configuration files for a module
// to include explicit provider source settings
type ZeroThirteenUpgradeCommand struct {
	Meta
}

func (c *ZeroThirteenUpgradeCommand) Run(args []string) int {
	args = c.Meta.process(args)
	flags := c.Meta.defaultFlagSet("0.13upgrade")
	flags.Usage = func() { c.Ui.Error(c.Help()) }
	if err := flags.Parse(args); err != nil {
		return 1
	}

	var diags tfdiags.Diagnostics

	var dir string
	args = flags.Args()
	switch len(args) {
	case 0:
		dir = "."
	case 1:
		dir = args[0]
	default:
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Too many arguments",
			"The command 0.13upgrade expects only a single argument, giving the directory containing the module to upgrade.",
		))
		c.showDiagnostics(diags)
		return 1
	}

	// Check for user-supplied plugin path
	var err error
	if c.pluginPath, err = c.loadPluginPath(); err != nil {
		c.Ui.Error(fmt.Sprintf("Error loading plugin path: %s", err))
		return 1
	}

	dir = c.normalizePath(dir)

	// Upgrade only if some configuration is present
	empty, err := configs.IsEmptyDir(dir)
	if err != nil {
		diags = diags.Append(fmt.Errorf("Error checking configuration: %s", err))
		return 1
	}
	if empty {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Not a module directory",
			fmt.Sprintf("The given directory %s does not contain any Terraform configuration files.", dir),
		))
		c.showDiagnostics(diags)
		return 1
	}

	// Set up the config loader and find all the config files
	loader, err := c.initConfigLoader()
	if err != nil {
		diags = diags.Append(err)
		c.showDiagnostics(diags)
		return 1
	}
	parser := loader.Parser()
	primary, overrides, hclDiags := parser.ConfigDirFiles(dir)
	diags = diags.Append(hclDiags)
	if diags.HasErrors() {
		c.Ui.Error(strings.TrimSpace("Failed to load configuration"))
		c.showDiagnostics(diags)
		return 1
	}

	// Load and parse all primary files
	files := make(map[string]*configs.File)
	for _, path := range primary {
		file, fileDiags := parser.LoadConfigFile(path)
		diags = diags.Append(fileDiags)
		if file != nil {
			files[path] = file
		}
	}
	if diags.HasErrors() {
		c.Ui.Error(strings.TrimSpace("Failed to load configuration"))
		c.showDiagnostics(diags)
		return 1
	}

	// FIXME: It's not clear what the correct behaviour is for upgrading
	// override files. For now, just log that we're ignoring the file.
	for _, path := range overrides {
		c.Ui.Warn(fmt.Sprintf("Ignoring override file %q: not implemented", path))
	}

	// Build up a list of required providers, uniquely by local name
	requiredProviders := make(map[string]*configs.RequiredProvider)
	var rewritePaths []string

	// Step 1: copy all explicit provider requirements across
	for path, file := range files {
		for _, rps := range file.RequiredProviders {
			rewritePaths = append(rewritePaths, path)
			for _, rp := range rps.RequiredProviders {
				if previous, exist := requiredProviders[rp.Name]; exist {
					diags = diags.Append(&hcl.Diagnostic{
						Summary:  "Duplicate required provider configuration",
						Detail:   fmt.Sprintf("Found duplicate required provider configuration for %q.Previously configured at %s", rp.Name, previous.DeclRange),
						Severity: hcl.DiagWarning,
						Context:  rps.DeclRange.Ptr(),
						Subject:  rp.DeclRange.Ptr(),
					})
				} else {
					// We're copying the struct here to ensure that any
					// mutation does not affect the original, if we rewrite
					// this file
					requiredProviders[rp.Name] = &configs.RequiredProvider{
						Name:        rp.Name,
						Source:      rp.Source,
						Type:        rp.Type,
						Requirement: rp.Requirement,
						DeclRange:   rp.DeclRange,
					}
				}
			}
		}
	}

	for _, file := range files {
		// Step 2: add missing provider requirements from provider blocks
		for _, p := range file.ProviderConfigs {
			// If no explicit provider configuration exists for the
			// provider configuration's local name, add one with a legacy
			// provider address.
			if _, exist := requiredProviders[p.Name]; !exist {
				requiredProviders[p.Name] = &configs.RequiredProvider{
					Name:        p.Name,
					Type:        addrs.NewLegacyProvider(p.Name),
					Requirement: p.Version,
				}
			}
		}

		// Step 3: add missing provider requirements from resources
		resources := [][]*configs.Resource{file.ManagedResources, file.DataResources}
		for _, rs := range resources {
			for _, r := range rs {
				// Find the appropriate provider local name for this resource
				var localName string

				// If there's a provider config, use that to determine the
				// local name. Otherwise use the implied provider local name
				// based on the resource's address.
				if r.ProviderConfigRef != nil {
					localName = r.ProviderConfigRef.Name
				} else {
					localName = r.Addr().ImpliedProvider()
				}

				// If no explicit provider configuration exists for this local
				// name, add one with a legacy provider address.
				if _, exist := requiredProviders[localName]; !exist {
					requiredProviders[localName] = &configs.RequiredProvider{
						Name: localName,
						Type: addrs.NewLegacyProvider(localName),
					}
				}
			}
		}
	}

	// We should now have a complete understanding of the provider requirements
	// stated in the config.  If there are any providers, attempt to detect
	// their sources, and rewrite the config.
	if len(requiredProviders) > 0 {
		detectDiags := c.detectProviderSources(requiredProviders)
		diags = diags.Append(detectDiags)
		if diags.HasErrors() {
			c.Ui.Error("Unable to detect sources for providers")
			c.showDiagnostics(diags)
			return 1
		}

		// Default output filename is "providers.tf"
		filename := "providers.tf"

		// Special case: if we only have one file with a required providers
		// block, output to that file instead.
		if len(rewritePaths) == 1 {
			filename = rewritePaths[0]

			// Remove this file from the list of paths we want to rewrite
			// later. Otherwise we'd delete the required providers block after
			// writing it.
			rewritePaths = nil
		}

		var out *hclwrite.File

		// If the output file doesn't exist, just create a new empty file
		if _, err := os.Stat(filename); os.IsNotExist(err) {
			out = hclwrite.NewEmptyFile()
		} else if err != nil {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Unable to read configuration file",
				fmt.Sprintf("Error when reading configuration file %q: %s", filename, err),
			))
			c.showDiagnostics(diags)
			return 1
		} else {
			// Configuration file already exists, so load and parse it
			config, err := ioutil.ReadFile(filename)
			if err != nil {
				diags = diags.Append(tfdiags.Sourceless(
					tfdiags.Error,
					"Unable to read configuration file",
					fmt.Sprintf("Error when reading configuration file %q: %s", filename, err),
				))
				c.showDiagnostics(diags)
				return 1
			}
			var parseDiags hcl.Diagnostics
			out, parseDiags = hclwrite.ParseConfig(config, filename, hcl.InitialPos)
			diags = diags.Append(parseDiags)
		}

		if diags.HasErrors() {
			c.showDiagnostics(diags)
			return 1
		}

		// Find all required_providers blocks, and store them alongside a map
		// back to the parent terraform block.
		var requiredProviderBlocks []*hclwrite.Block
		parentBlocks := make(map[*hclwrite.Block]*hclwrite.Block)
		root := out.Body()
		for _, rootBlock := range root.Blocks() {
			if rootBlock.Type() != "terraform" {
				continue
			}
			for _, childBlock := range rootBlock.Body().Blocks() {
				if childBlock.Type() == "required_providers" {
					requiredProviderBlocks = append(requiredProviderBlocks, childBlock)
					parentBlocks[childBlock] = rootBlock
				}
			}
		}

		// First required provider block, and the rest found in this file.
		var first *hclwrite.Block
		var rest []*hclwrite.Block

		if len(requiredProviderBlocks) > 0 {
			// If we already have one or more required provider blocks, we'll rewrite
			// the first one, and remove the rest.
			first, rest = requiredProviderBlocks[0], requiredProviderBlocks[1:]
		} else {
			// Otherwise, find or a create a terraform block, and add a new
			// empty required providers block to it.
			var tfBlock *hclwrite.Block
			for _, rootBlock := range root.Blocks() {
				if rootBlock.Type() == "terraform" {
					tfBlock = rootBlock
					break
				}
			}
			if tfBlock == nil {
				tfBlock = root.AppendNewBlock("terraform", nil)
			}
			first = tfBlock.Body().AppendNewBlock("required_providers", nil)
		}

		// Find the body of the first block to prepare for rewriting it
		body := first.Body()

		// Build a sorted list of provider local names, for consistent ordering
		var localNames []string
		for localName := range requiredProviders {
			localNames = append(localNames, localName)
		}
		sort.Strings(localNames)

		// Populate the required providers block
		for _, localName := range localNames {
			requiredProvider := requiredProviders[localName]
			var attributes = make(map[string]cty.Value)

			if !requiredProvider.Type.IsZero() {
				attributes["source"] = cty.StringVal(requiredProvider.Type.String())
			}

			if version := requiredProvider.Requirement.Required.String(); version != "" {
				attributes["version"] = cty.StringVal(version)
			}

			var attributesObject cty.Value
			if len(attributes) > 0 {
				attributesObject = cty.ObjectVal(attributes)
			} else {
				attributesObject = cty.EmptyObjectVal
			}
			body.SetAttributeValue(localName, attributesObject)

			// If we don't have a source attribute, manually construct a commented
			// block explaining what to do
			if _, hasSource := attributes["source"]; !hasSource {
				// Generate the token stream for the required provider
				rp := body.GetAttribute(localName)
				expr := rp.Expr().BuildTokens(nil)

				// Paritition the tokens into before and after the opening paren
				before, after := partitionTokensAfter(expr, hclsyntax.TokenOBrace)

				// If the value is an empty object, add a newline between the
				// braces so that the comment is not on the same line as either
				// brace.
				if len(before) == 1 && len(after) == 1 {
					newline := &hclwrite.Token{
						Type:  hclsyntax.TokenNewline,
						Bytes: []byte{'\n'},
					}
					after = append(hclwrite.Tokens{newline}, after...)
				}

				// Generate the comment and insert it at the start of the object
				comment := noSourceDetectedComment(localName)
				commentedBlock := append(before, comment...)
				commentedBlock = append(commentedBlock, after...)

				// Set the required provider object to this raw token stream
				body.SetAttributeRaw(localName, commentedBlock)
			}
		}

		// Remove the rest of the blocks (and the parent block, if it's empty)
		for _, rpBlock := range rest {
			tfBlock := parentBlocks[rpBlock]
			tfBody := tfBlock.Body()
			tfBody.RemoveBlock(rpBlock)

			// If the terraform block has no blocks and no attributes, it's
			// basically empty (aside from comments and whitespace), so it's
			// more useful to remove it than leave it in.
			if len(tfBody.Blocks()) == 0 && len(tfBody.Attributes()) == 0 {
				root.RemoveBlock(tfBlock)
			}
		}

		// Write the config back to the file
		f, err := os.OpenFile(filename, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Unable to open configuration file for writing",
				fmt.Sprintf("Error when reading configuration file %q: %s", filename, err),
			))
			c.showDiagnostics(diags)
			return 1
		}
		_, err = out.WriteTo(f)
		if err != nil {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Unable to rewrite configuration file",
				fmt.Sprintf("Error when rewriting configuration file %q: %s", filename, err),
			))
			c.showDiagnostics(diags)
			return 1
		}

		// After successfully writing the new configuration, remove all other
		// required provider blocks from remaining configuration files.
		for _, path := range rewritePaths {
			// Read and parse the existing file
			config, err := ioutil.ReadFile(path)
			if err != nil {
				diags = diags.Append(tfdiags.Sourceless(
					tfdiags.Error,
					"Unable to read configuration file",
					fmt.Sprintf("Error when reading configuration file %q: %s", filename, err),
				))
				c.showDiagnostics(diags)
				return 1
			}
			file, parseDiags := hclwrite.ParseConfig(config, filename, hcl.InitialPos)
			diags = diags.Append(parseDiags)
			if diags.HasErrors() {
				c.showDiagnostics(diags)
				return 1
			}

			// Find and remove all terraform.required_providers blocks
			root := file.Body()
			for _, rootBlock := range root.Blocks() {
				if rootBlock.Type() != "terraform" {
					continue
				}
				tfBody := rootBlock.Body()
				for _, childBlock := range tfBody.Blocks() {
					if childBlock.Type() == "required_providers" {
						rootBlock.Body().RemoveBlock(childBlock)

						// If the terraform block is now empty, remove it
						if len(tfBody.Blocks()) == 0 && len(tfBody.Attributes()) == 0 {
							root.RemoveBlock(rootBlock)
						}
					}
				}
			}

			// Write the config back to the file
			f, err := os.OpenFile(path, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				diags = diags.Append(tfdiags.Sourceless(
					tfdiags.Error,
					"Unable to open configuration file for writing",
					fmt.Sprintf("Error when reading configuration file %q: %s", filename, err),
				))
				c.showDiagnostics(diags)
				return 1
			}
			_, err = file.WriteTo(f)
			if err != nil {
				diags = diags.Append(tfdiags.Sourceless(
					tfdiags.Error,
					"Unable to rewrite configuration file",
					fmt.Sprintf("Error when rewriting configuration file %q: %s", filename, err),
				))
				c.showDiagnostics(diags)
				return 1
			}
		}
	}

	c.showDiagnostics(diags)
	if diags.HasErrors() {
		return 1
	}

	if len(diags) != 0 {
		c.Ui.Output(`-----------------------------------------------------------------------------`)
	}
	c.Ui.Output(c.Colorize().Color(`
[bold][green]Upgrade complete![reset]

Use your version control system to review the proposed changes, make any
necessary adjustments, and then commit.
`))

	return 0
}

// For providers which need a source attribute, detect the source
func (c *ZeroThirteenUpgradeCommand) detectProviderSources(requiredProviders map[string]*configs.RequiredProvider) tfdiags.Diagnostics {
	source := c.providerInstallSource()
	var diags tfdiags.Diagnostics

	for name, rp := range requiredProviders {
		// If there's already an explicit source, skip it
		if rp.Source != "" {
			continue
		}

		// Construct a legacy provider FQN using the existing addr's type. This
		// is necessary because the config parser for required providers
		// constructs a default provider FQN for configurations with no source.
		// For this tool specifically we want to treat those as legacy
		// providers, so that we can look up the namespace on the registry.
		addr := addrs.NewLegacyProvider(rp.Type.Type)
		p, err := getproviders.LookupLegacyProvider(addr, source)
		if err == nil {
			rp.Type = p
		} else {
			if _, ok := err.(getproviders.ErrProviderNotKnown); ok {
				// Setting the provider address to a zero value struct
				// indicates that there is no known FQN for this provider,
				// which will cause us to write an explanatory comment in the
				// HCL output advising the user what to do about this.
				rp.Type = addrs.Provider{}
			}
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Warning,
				"Could not detect provider source",
				fmt.Sprintf("Error looking up provider source for %q: %s", name, err),
			))
		}
	}

	return diags
}

// Take a list of tokens and a separator token, and return two lists: one up to
// and including the first instance of the separator, and the rest of the
// tokens. If the separator is not present, return the entire list in the first
// return value.
func partitionTokensAfter(tokens hclwrite.Tokens, separator hclsyntax.TokenType) (hclwrite.Tokens, hclwrite.Tokens) {
	for i := 0; i < len(tokens); i++ {
		if tokens[i].Type == separator {
			return tokens[0 : i+1], tokens[i+1:]
		}
	}

	return tokens, nil
}

// Generate a list of tokens for a comment explaining that a provider source
// could not be detected.
func noSourceDetectedComment(name string) hclwrite.Tokens {
	comment := fmt.Sprintf(`# TF-UPGRADE-TODO
#
# No source detected for this provider. You must add a source address
# in the following format:
#
# source = "your.domain.com/organization/%s"
#
# For more information, see the provider source documentation:
#
# https://www.terraform.io/docs/configuration/providers.html#provider-source`, name)

	var tokens hclwrite.Tokens
	for _, line := range strings.Split(comment, "\n") {
		tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenNewline, Bytes: []byte{'\n'}})
		tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenComment, Bytes: []byte(line)})
	}
	return tokens
}

func (c *ZeroThirteenUpgradeCommand) Help() string {
	helpText := `
Usage: terraform 0.13upgrade [module-dir]

  Generates a "providers.tf" configuration file which includes source
  configuration for every non-default provider.
`
	return strings.TrimSpace(helpText)
}

func (c *ZeroThirteenUpgradeCommand) Synopsis() string {
	return "Rewrites pre-0.13 module source code for v0.13"
}
