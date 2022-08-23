package bp2build

import (
	"encoding/json"
	"reflect"
	"strings"

	"android/soong/android"
	cc_config "android/soong/cc/config"
	java_config "android/soong/java/config"

	"android/soong/apex"

	"github.com/google/blueprint/proptools"
)

type BazelFile struct {
	Dir      string
	Basename string
	Contents string
}

func CreateSoongInjectionFiles(cfg android.Config, metrics CodegenMetrics) []BazelFile {
	var files []BazelFile

	files = append(files, newFile("cc_toolchain", GeneratedBuildFileName, "")) // Creates a //cc_toolchain package.
	files = append(files, newFile("cc_toolchain", "constants.bzl", cc_config.BazelCcToolchainVars(cfg)))

	files = append(files, newFile("java_toolchain", GeneratedBuildFileName, "")) // Creates a //java_toolchain package.
	files = append(files, newFile("java_toolchain", "constants.bzl", java_config.BazelJavaToolchainVars(cfg)))

	files = append(files, newFile("apex_toolchain", GeneratedBuildFileName, "")) // Creates a //apex_toolchain package.
	files = append(files, newFile("apex_toolchain", "constants.bzl", apex.BazelApexToolchainVars()))

	files = append(files, newFile("metrics", "converted_modules.txt", strings.Join(metrics.convertedModules, "\n")))

	files = append(files, newFile("product_config", "soong_config_variables.bzl", cfg.Bp2buildSoongConfigDefinitions.String()))

	files = append(files, newFile("product_config", "arch_configuration.bzl", android.StarlarkArchConfigurations()))

	apiLevelsContent, err := json.Marshal(android.GetApiLevelsMap(cfg))
	if err != nil {
		panic(err)
	}
	files = append(files, newFile("api_levels", GeneratedBuildFileName, `exports_files(["api_levels.json"])`))
	files = append(files, newFile("api_levels", "api_levels.json", string(apiLevelsContent)))
	files = append(files, newFile("api_levels", "api_levels.bzl", android.StarlarkApiLevelConfigs(cfg)))

	return files
}

func convertedModules(convertedModules []string) string {
	return strings.Join(convertedModules, "\n")
}

func CreateBazelFiles(
	cfg android.Config,
	ruleShims map[string]RuleShim,
	buildToTargets map[string]BazelTargets,
	mode CodegenMode) []BazelFile {

	var files []BazelFile

	if mode == QueryView {
		// Write top level WORKSPACE.
		files = append(files, newFile("", "WORKSPACE", ""))

		// Used to denote that the top level directory is a package.
		files = append(files, newFile("", GeneratedBuildFileName, ""))

		files = append(files, newFile(bazelRulesSubDir, GeneratedBuildFileName, ""))

		// These files are only used for queryview.
		files = append(files, newFile(bazelRulesSubDir, "providers.bzl", providersBzl))

		for bzlFileName, ruleShim := range ruleShims {
			files = append(files, newFile(bazelRulesSubDir, bzlFileName+".bzl", ruleShim.content))
		}
		files = append(files, newFile(bazelRulesSubDir, "soong_module.bzl", generateSoongModuleBzl(ruleShims)))
	}

	files = append(files, createBuildFiles(buildToTargets, mode)...)

	return files
}

func createBuildFiles(buildToTargets map[string]BazelTargets, mode CodegenMode) []BazelFile {
	files := make([]BazelFile, 0, len(buildToTargets))
	for _, dir := range android.SortedStringKeys(buildToTargets) {
		targets := buildToTargets[dir]
		targets.sort()

		var content string
		if mode == Bp2Build {
			content = `# READ THIS FIRST:
# This file was automatically generated by bp2build for the Bazel migration project.
# Feel free to edit or test it, but do *not* check it into your version control system.
`
			if targets.hasHandcraftedTargets() {
				// For BUILD files with both handcrafted and generated targets,
				// don't hardcode actual content, like package() declarations.
				// Leave that responsibility to the checked-in BUILD file
				// instead.
				content += `# This file contains generated targets and handcrafted targets that are manually managed in the source tree.`
			} else {
				// For fully-generated BUILD files, hardcode the default visibility.
				content += "package(default_visibility = [\"//visibility:public\"])"
			}
			content += "\n"
			content += targets.LoadStatements()
		} else if mode == QueryView {
			content = soongModuleLoad
		}
		if content != "" {
			// If there are load statements, add a couple of newlines.
			content += "\n\n"
		}
		content += targets.String()
		files = append(files, newFile(dir, GeneratedBuildFileName, content))
	}
	return files
}

func newFile(dir, basename, content string) BazelFile {
	return BazelFile{
		Dir:      dir,
		Basename: basename,
		Contents: content,
	}
}

const (
	bazelRulesSubDir = "build/bazel/queryview_rules"

	// additional files:
	//  * workspace file
	//  * base BUILD file
	//  * rules BUILD file
	//  * rules providers.bzl file
	//  * rules soong_module.bzl file
	numAdditionalFiles = 5
)

var (
	// Certain module property names are blocklisted/ignored here, for the reasons commented.
	ignoredPropNames = map[string]bool{
		"name":       true, // redundant, since this is explicitly generated for every target
		"from":       true, // reserved keyword
		"in":         true, // reserved keyword
		"size":       true, // reserved for tests
		"arch":       true, // interface prop type is not supported yet.
		"multilib":   true, // interface prop type is not supported yet.
		"target":     true, // interface prop type is not supported yet.
		"visibility": true, // Bazel has native visibility semantics. Handle later.
		"features":   true, // There is already a built-in attribute 'features' which cannot be overridden.
		"for":        true, // reserved keyword, b/233579439
	}
)

func shouldGenerateAttribute(prop string) bool {
	return !ignoredPropNames[prop]
}

func shouldSkipStructField(field reflect.StructField) bool {
	if field.PkgPath != "" && !field.Anonymous {
		// Skip unexported fields. Some properties are
		// internal to Soong only, and these fields do not have PkgPath.
		return true
	}
	// fields with tag `blueprint:"mutated"` are exported to enable modification in mutators, etc
	// but cannot be set in a .bp file
	if proptools.HasTag(field, "blueprint", "mutated") {
		return true
	}
	return false
}

// FIXME(b/168089390): In Bazel, rules ending with "_test" needs to be marked as
// testonly = True, forcing other rules that depend on _test rules to also be
// marked as testonly = True. This semantic constraint is not present in Soong.
// To work around, rename "*_test" rules to "*_test_".
func canonicalizeModuleType(moduleName string) string {
	if strings.HasSuffix(moduleName, "_test") {
		return moduleName + "_"
	}

	return moduleName
}
