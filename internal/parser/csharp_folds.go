package parser

// CSharpBCLKeywordFolds maps each BCL alias spelling (plus `dynamic`,
// which erases to object) onto the C# keyword form of the same
// underlying type. The ONE declarative fold table both sides of the
// dispatch gate consult: the extractor's type-argument canonicalization
// (languages.csharpCanonicalTypeArg) and the resolver's global-alias
// comparable-forms expansion (csharpAliasComparableForms). A fold
// present on one side only makes the alias refusal miss the folded
// spelling - `global using @dynamic = ...` stamped object while the
// refusal only knew Object->object, and a whole fan-out was suppressed.
// It lives here, below both packages, because the resolver cannot
// import languages (the languages test binary would cycle back through
// the resolver).
var CSharpBCLKeywordFolds = map[string]string{
	"String":  "string",
	"Boolean": "bool",
	"Byte":    "byte",
	"SByte":   "sbyte",
	"Char":    "char",
	"Decimal": "decimal",
	"Double":  "double",
	"Single":  "float",
	"Int16":   "short",
	"UInt16":  "ushort",
	"Int32":   "int",
	"UInt32":  "uint",
	"Int64":   "long",
	"UInt64":  "ulong",
	"Object":  "object",
	// dynamic erases to object - the two spellings construct over the
	// same underlying type, and folding can only CREATE matches.
	"dynamic": "object",
	"IntPtr":  "nint",
	"UIntPtr": "nuint",
}
