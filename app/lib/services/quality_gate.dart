import 'dart:math' as math;
import 'dart:typed_data';

import 'package:image/image.dart' as img;

/// Výsledek kontroly jedné fotky.
class QualityResult {
  final bool accepted;
  final List<String> errors; // blokují upload
  final List<String> warnings; // jen varují
  final int width;
  final int height;
  final double sharpness;

  const QualityResult({
    required this.accepted,
    required this.errors,
    required this.warnings,
    required this.width,
    required this.height,
    required this.sharpness,
  });

  bool get hasIssues => errors.isNotEmpty || warnings.isNotEmpty;
}

/// Quality gate, který běží na zařízení PŘED uploadem (§3.1 plánu).
///
/// Proč na zařízení: šetří round-tripy a GPU čas, a je to lepší privacy
/// story do App Store review — nekvalitní fotka opustí telefon jen tehdy,
/// když má smysl ji zpracovávat.
///
/// Fáze 0 dělá jen to, co jde bez modelu: rozlišení a ostrost. Detekce
/// obličeje, počet obličejů, natočení hlavy a okluze potřebují MediaPipe
/// a přijdou ve Fázi 2 — do té doby se na ně jen upozorňuje textem v UI.
class QualityGate {
  /// Minimální strana fotky. Plán mluví o 512 px na obličej; než bude
  /// detekce obličeje, měříme celou fotku a držíme rezervu.
  static const int minImageSide = 1024;

  /// Práh Laplacian variance. Pod ním je fotka rozmazaná.
  ///
  /// Hodnota je empirická a bude potřebovat doladit na reálných fotkách
  /// z telefonů (Fáze 3 zmiňuje tuning pass na 30 obličejích).
  static const double minSharpness = 55.0;

  /// Nad tímhle rozlišením se fotka před analýzou zmenší — Laplacian
  /// na 12Mpx fotce trvá na telefonu déle, než uživatel vydrží.
  static const int analysisMaxSide = 1024;

  /// Zkontroluje jednu fotku. Vrací null, když se soubor nepodaří dekódovat.
  static QualityResult? check(Uint8List bytes) {
    final decoded = img.decodeImage(bytes);
    if (decoded == null) return null;

    final errors = <String>[];
    final warnings = <String>[];

    final shortSide = math.min(decoded.width, decoded.height);
    if (shortSide < minImageSide) {
      errors.add(
        'Fotka je malá (${decoded.width}×${decoded.height} px). '
        'Potřebuju aspoň ${minImageSide}px na kratší straně.',
      );
    }

    final analysed = shortSide > analysisMaxSide
        ? img.copyResize(
            decoded,
            width: decoded.width >= decoded.height ? null : analysisMaxSide,
            height: decoded.height > decoded.width ? null : analysisMaxSide,
            interpolation: img.Interpolation.average,
          )
        : decoded;

    final sharpness = _laplacianVariance(analysed);
    if (sharpness < minSharpness) {
      errors.add('Fotka je rozmazaná. Zkus ji vyfotit znovu, líp zaostřenou.');
    } else if (sharpness < minSharpness * 1.6) {
      warnings.add('Fotka je jen tak tak ostrá — ostřejší by dala lepší výsledek.');
    }

    final aspect = decoded.width / decoded.height;
    if (aspect < 0.5 || aspect > 2.0) {
      warnings.add('Hodně protáhlý formát. Nejlíp funguje portrét nebo čtverec.');
    }

    return QualityResult(
      accepted: errors.isEmpty,
      errors: errors,
      warnings: warnings,
      width: decoded.width,
      height: decoded.height,
      sharpness: sharpness,
    );
  }

  /// Rozptyl Laplaciánu — standardní míra ostrosti.
  ///
  /// Ostrá fotka má hodně silných přechodů, takže druhá derivace hodně
  /// kolísá a rozptyl je vysoký. Rozmazaná fotka má přechody ploché.
  static double _laplacianVariance(img.Image src) {
    final gray = img.grayscale(src);
    final w = gray.width;
    final h = gray.height;
    if (w < 3 || h < 3) return 0;

    // Luma vytáhneme jednou dopředu — volání getPixel v vnitřní smyčce
    // je v Dartu drahé a tohle běží na hlavním vlákně UI.
    final lum = Float32List(w * h);
    for (var y = 0; y < h; y++) {
      for (var x = 0; x < w; x++) {
        lum[y * w + x] = gray.getPixel(x, y).r.toDouble();
      }
    }

    // Kernel [[0,1,0],[1,-4,1],[0,1,0]]
    var sum = 0.0;
    var sumSq = 0.0;
    var n = 0;

    for (var y = 1; y < h - 1; y++) {
      for (var x = 1; x < w - 1; x++) {
        final i = y * w + x;
        final v = lum[i - w] + lum[i + w] + lum[i - 1] + lum[i + 1] - 4 * lum[i];
        sum += v;
        sumSq += v * v;
        n++;
      }
    }

    if (n == 0) return 0;
    final mean = sum / n;
    return (sumSq / n) - (mean * mean);
  }
}
