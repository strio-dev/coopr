{
  pkgs,
  nix2container,
  coopr,
  artifact ? "image",
  exporter ? pkgs.callPackage ./exporter.nix { },
}:
{
  name,
  tag ? null,
  packages ? [ ],
  copyToRoot ? [ ],
  config ? { },
  perms ? [ ],
  maxLayers ? 1,
  components ? [ ],
  definition ? null,
  target ? null,
  args ? { },
  context ? null,
  cooprBinary ? "${coopr}/bin/coopr",
}:
let
  inherit (pkgs) lib;
  componentArtifact = artifact == "component";
  apiName = if componentArtifact then "coopr.buildComponent" else "coopr.buildImage";
  configFields = [
    "User"
    "ExposedPorts"
    "Env"
    "Entrypoint"
    "Cmd"
    "Volumes"
    "WorkingDir"
    "Labels"
    "StopSignal"
    "ArgsEscaped"
  ];
  host = pkgs.stdenv.hostPlatform;
  supported = host.isLinux && (host.isx86_64 || host.isAarch64);
  architecture = if host.isx86_64 then "amd64" else "arm64";
  platform = "linux/${architecture}";
  isInput =
    value:
    builtins.isPath value
    || lib.isDerivation value
    || (
      builtins.isString value
      && builtins.hasContext value
      && lib.hasPrefix "${builtins.storeDir}/" value
      # String context alone does not snapshot an arbitrary host path. Reject
      # lexical escapes instead of treating unrelated dependency metadata as
      # evidence that the selected path is a Nix input.
      && lib.all (part: part != "" && part != "." && part != "..") (
        lib.splitString "/" (lib.removePrefix "${builtins.storeDir}/" value)
      )
    );
  validArgs =
    value:
    builtins.isAttrs value
    && lib.all (
      key: builtins.match "[a-zA-Z_][a-zA-Z0-9_]*" key != null && builtins.isString value.${key}
    ) (builtins.attrNames value);
  validReference =
    value:
    builtins.isString value
    && builtins.match "(sha256:[0-9a-f]{64}|[^[:space:]@$]+@sha256:[0-9a-f]{64})" value != null;
  validComponent =
    value:
    builtins.isAttrs value
    && lib.all (
      key:
      builtins.elem key [
        "ref"
        "args"
        "archive"
        "bundle"
      ]
    ) (builtins.attrNames value)
    && value ? ref
    && validReference value.ref
    && validArgs (value.args or { })
    && ((value.archive or null) == null || isInput value.archive)
    && (
      (value.bundle or null) == null
      || (componentArtifact && (value.archive or null) != null && isInput value.bundle)
    );
  selectedComponents = lib.imap0 (
    index: component:
    let
      archive = component.archive or null;
      archivePath = if archive == null then null else "${archive}";
      digest = lib.last (lib.splitString "@" component.ref);
      # Explicit archive-specific names retain the loaded delivery selection
      # when several valid bindings share the same immutable component root.
      archiveName =
        if archivePath == null then
          null
        else
          "${
            if componentArtifact then "coopr.invalid/nix/" else "coopr-nix-"
          }${builtins.hashString "sha256" archivePath}:archive";
    in
    {
      inherit
        index
        archive
        archivePath
        digest
        archiveName
        ;
      inherit (component) ref;
      args = component.args or { };
      bundlePath = if (component.bundle or null) == null then null else "${component.bundle}";
      invocationReference = if archiveName == null then component.ref else "${archiveName}@${digest}";
    }
  ) components;
  bindings = lib.concatMap (
    component:
    lib.imap0 (index: key: {
      name = "COOPR_NIX_COMPONENT_${toString component.index}_${toString index}";
      original = key;
      value = component.args.${key};
      component = component.index;
    }) (builtins.attrNames component.args)
  ) selectedComponents;
  nativeArgs =
    args
    // builtins.listToAttrs (
      map (binding: {
        inherit (binding) name value;
      }) (if componentArtifact then [ ] else bindings)
    );
  # Published EXTEND instructions expand when the component is invoked. Escape
  # native expansion once so Nix-declared values remain literal after export.
  nativeLiteral = value: builtins.toJSON (lib.replaceStrings [ "\\" "$" ] [ "\\\\" "\\$" ] value);
  generatedDefinition = pkgs.writeText "coopr-nix-${artifact}.coopr" (
    (
      if componentArtifact then
        ''
          from "coopr-nix" as="coopr-nix-input" {
          }
          package as="coopr-nix-closure" {
            copy "/" "/" from="coopr-nix-input"
          }
          extend {
            copy "/" "/" from="coopr-nix-closure"
        ''
      else
        lib.concatMapStringsSep "\n" (binding: "arg ${builtins.toJSON binding.name}") bindings
        + ''

          from "coopr-nix" {
        ''
        + lib.concatMapStringsSep "\n" (binding: "  arg ${builtins.toJSON binding.name}") bindings
    )
    + "\n"
    + lib.concatMapStringsSep "\n" (
      component:
      let
        componentBindings = builtins.filter (binding: binding.component == component.index) bindings;
      in
      "  component ${builtins.toJSON component.invocationReference} {\n"
      + lib.concatMapStringsSep "\n" (
        binding:
        "    arg ${builtins.toJSON binding.original} ${
              if componentArtifact then
                nativeLiteral binding.value
              else
                builtins.toJSON ("\${" + binding.name + "}")
            }"
      ) componentBindings
      + "\n  }"
    ) selectedComponents
    + "\n}\n"
  );
  selectedDefinition = if definition == null then generatedDefinition else definition;
  selectedContext = if context == null then pkgs.emptyDirectory else context;
  # Retain package-valued argument closures in the image, not merely in the
  # launcher, using upstream's ignored context-holder pattern.
  argumentJSON = builtins.toJSON {
    inherit args;
    components = map (component: component.args) selectedComponents;
  };
  argumentReferences = pkgs.writeText "coopr-nix-argument-inputs.json" argumentJSON;
  argumentLayers = lib.optional (builtins.hasContext argumentJSON) (
    nix2container.buildLayer {
      inherit perms;
      deps = [ argumentReferences ];
      ignore = argumentReferences;
    }
  );
  packageRoot = pkgs.buildEnv {
    name = "coopr-image-packages";
    paths = packages;
    pathsToLink = [ "/bin" ];
  };
  image = nix2container.buildImage {
    inherit
      name
      tag
      perms
      maxLayers
      ;
    arch = architecture;
    layers = argumentLayers;
    copyToRoot = lib.optional (packages != [ ]) packageRoot ++ lib.toList copyToRoot;
    # Initial configuration only: native Coopr composition owns the final image.
    config = {
      Env = [ "PATH=/bin" ];
    }
    // config;
  };
  oci =
    pkgs.runCommand "coopr-image-${builtins.baseNameOf name}-oci"
      {
        nativeBuildInputs = [ exporter ];
      }
      ''
        coopr-nix-export ${image} "$out"
      '';
  inputImage = image // {
    inherit oci;
  };
  recipe = pkgs.writeText "coopr-nix-${artifact}-recipe.json" (
    builtins.toJSON {
      definition = "${selectedDefinition}";
      context = "${selectedContext}";
      inherit platform target;
      args = nativeArgs;
      input = "${oci}";
      finalTag = if tag == null then null else "${name}:${tag}";
      components = map (component: {
        inherit (component)
          ref
          digest
          archiveName
          invocationReference
          ;
        archive = component.archivePath;
        bundle = component.bundlePath;
      }) selectedComponents;
    }
  );
  launcherName = lib.strings.sanitizeDerivationName "coopr-build-${lib.optionalString componentArtifact "component-"}${builtins.baseNameOf name}";
in
assert lib.assertMsg (builtins.elem artifact [
  "image"
  "component"
]) "invalid internal Coopr Nix artifact kind";
assert lib.assertMsg
  (
    builtins.isAttrs config && lib.all (key: builtins.elem key configFields) (builtins.attrNames config)
  )
  "${apiName} config must use supported OCI image configuration fields; use native Coopr instructions for additional configuration";
assert lib.assertMsg (!componentArtifact || config == { })
  "coopr.buildComponent does not inherit OCI image config; declare component configuration in native EXTEND instructions";
assert lib.assertMsg supported "${apiName} supports x86_64-linux and aarch64-linux package sets";
assert lib.assertMsg (builtins.isString name && name != "") "${apiName} requires a nonempty name";
assert lib.assertMsg (
  tag == null || builtins.isString tag
) "${apiName} tag must be null or a string";
assert lib.assertMsg (
  builtins.isList components && lib.all validComponent components
) "${apiName} components require digest-pinned ref, string args and optional Nix archive input";
assert lib.assertMsg (validArgs args) "${apiName} args must be named strings";
assert lib.assertMsg (
  definition == null || isInput definition
) "${apiName} definition must be a Nix input";
assert lib.assertMsg (context == null || isInput context) "${apiName} context must be a Nix input";
assert lib.assertMsg (
  builtins.isString cooprBinary && isInput cooprBinary
) "${apiName} cooprBinary must select an executable path from a Nix input";
assert lib.assertMsg (
  definition == null || components == [ ]
) "${apiName} definition and generated components are mutually exclusive";
assert lib.assertMsg (
  definition != null || (args == { } && target == null)
) "${apiName} args and target require an explicit definition";
assert lib.assertMsg (
  target == null || (builtins.isString target && target != "")
) "${apiName} target must be null or a nonempty string";
pkgs.writeShellApplication {
  name = launcherName;
  runtimeInputs = [
    pkgs.jq
    pkgs.coreutils
  ];
  text = ''
    readonly coopr_nix_recipe=${lib.escapeShellArg recipe}
    readonly coopr_nix_default_binary=${lib.escapeShellArg cooprBinary}
    readonly coopr_nix_kind=${lib.escapeShellArg artifact}
  ''
  + builtins.readFile ./image-builder.sh;
  meta.mainProgram = launcherName;
  passthru = {
    inherit
      inputImage
      platform
      recipe
      artifact
      ;
    definition = selectedDefinition;
  };
}
