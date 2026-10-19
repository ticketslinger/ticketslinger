import wasmUrl from "zxing-wasm/reader/zxing_reader.wasm?url";

export type Detect = (frame: HTMLVideoElement) => Promise<string[]>;

// Not in TypeScript's DOM types yet.
type BarcodeDetectorLike = { detect(src: CanvasImageSource): Promise<{ rawValue: string }[]> };
type BarcodeDetectorClass = {
  new (opts: { formats: string[] }): BarcodeDetectorLike;
  getSupportedFormats(): Promise<string[]>;
};

/**
 * The browser's own QR reader when it has one; otherwise zxing-wasm, loaded
 * only now and fetched from our own /assets/ (never a CDN).
 */
export async function createDetector(): Promise<Detect> {
  const Native = (globalThis as { BarcodeDetector?: BarcodeDetectorClass }).BarcodeDetector;
  if (
    Native &&
    (await Native.getSupportedFormats().catch((): string[] => [])).includes("qr_code")
  ) {
    const d = new Native({ formats: ["qr_code"] });
    return async (frame) => (await d.detect(frame)).map((b) => b.rawValue);
  }

  const { prepareZXingModule, readBarcodes } = await import("zxing-wasm/reader");
  await prepareZXingModule({
    overrides: {
      locateFile: (p: string, prefix: string) => (p.endsWith(".wasm") ? wasmUrl : prefix + p),
    },
    fireImmediately: true,
  });
  const canvas = document.createElement("canvas");
  const ctx = canvas.getContext("2d", { willReadFrequently: true });
  return async (frame) => {
    const w = frame.videoWidth;
    const h = frame.videoHeight;
    if (!ctx || !w || !h) return [];
    canvas.width = w;
    canvas.height = h;
    ctx.drawImage(frame, 0, 0, w, h);
    const found = await readBarcodes(ctx.getImageData(0, 0, w, h), { formats: ["QRCode"] });
    return found.filter((b) => b.isValid).map((b) => b.text);
  };
}
