import 'package:flutter/material.dart';

import 'screens/upload_screen.dart';
import 'services/api_client.dart';

/// Adresa API. Přepíná se při buildu:
///   flutter run --dart-define=AVATAR_API_URL=http://192.168.88.66:8092
///
/// Výchozí 10.0.2.2 je host viděný z Android emulátoru; na fyzickém
/// telefonu je potřeba adresu předat explicitně.
const String kApiUrl = String.fromEnvironment(
  'AVATAR_API_URL',
  defaultValue: 'http://10.0.2.2:8092',
);

void main() => runApp(const AvatarStudioApp());

class AvatarStudioApp extends StatefulWidget {
  const AvatarStudioApp({super.key});

  @override
  State<AvatarStudioApp> createState() => _AvatarStudioAppState();
}

class _AvatarStudioAppState extends State<AvatarStudioApp> {
  late final ApiClient _api = ApiClient(baseUrl: kApiUrl);

  @override
  void dispose() {
    _api.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Avatar Studio',
      debugShowCheckedModeBanner: false,
      theme: ThemeData(
        colorScheme: ColorScheme.fromSeed(
          seedColor: const Color(0xFF6750A4),
          brightness: Brightness.dark,
        ),
        useMaterial3: true,
      ),
      home: UploadScreen(api: _api),
    );
  }
}
