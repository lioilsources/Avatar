/// Preset tak, jak ho vrací API (zkrácené DTO, ne celý recept).
class Preset {
  final String id;
  final int version;
  final String category;
  final Map<String, String> label;
  final List<String> supportsAttrs;
  final String profile;
  final String licenseTier;

  const Preset({
    required this.id,
    required this.version,
    required this.category,
    required this.label,
    required this.supportsAttrs,
    required this.profile,
    required this.licenseTier,
  });

  /// Název pro UI. Preferuje češtinu, spadne na angličtinu, pak na id.
  String get displayName => label['cs'] ?? label['en'] ?? id;

  factory Preset.fromJson(Map<String, dynamic> j) => Preset(
        id: j['id'] as String,
        version: j['version'] as int? ?? 1,
        category: j['category'] as String? ?? 'realistic',
        label: (j['label'] as Map<String, dynamic>? ?? const {})
            .map((k, v) => MapEntry(k, v as String)),
        supportsAttrs: (j['supports_attrs'] as List<dynamic>? ?? const [])
            .map((e) => e as String)
            .toList(growable: false),
        profile: j['profile'] as String? ?? 'final',
        licenseTier: j['license_tier'] as String? ?? 'verify',
      );
}
