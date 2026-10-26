// Camera/library normalization. No uploads or persistent browser storage here.
(function (root) {
  'use strict';
  const MAX_COUNT = 4, MAX_INPUT = 16 * 1024 * 1024, MAX_BYTES = 8 * 1024 * 1024;
  function validate(file) {
    if (!file || !file.size) throw new Error('This photo is empty.');
    if (file.size > MAX_INPUT) throw new Error('Choose photos smaller than 16 MiB each.');
    if (file.type && !file.type.startsWith('image/')) throw new Error('Choose an image from Camera or Photos.');
  }
  function size(width, height, edge = 2560) {
    if (!width || !height) throw new Error('This image has no dimensions.');
    const scale = Math.min(1, edge / Math.max(width, height));
    return [Math.max(1, Math.round(width * scale)), Math.max(1, Math.round(height * scale))];
  }
  async function prepare(file) {
    validate(file);
    let timer, reader;
    const image = new Image();
    try {
      await new Promise((resolve, reject) => {
        timer = setTimeout(() => { if (reader) reader.abort(); reject(new Error('Photo could not be decoded within 15 s. Try a JPEG or screenshot.')); }, 15000);
        reader = new FileReader();
        reader.onerror = () => reject(new Error('Cannot read this photo.'));
        image.onerror = () => reject(new Error('Cannot open this format. Choose a JPEG or screenshot.'));
        image.onload = resolve;
        reader.onload = () => { image.src = reader.result; };
        reader.readAsDataURL(file);
      });
    } finally { clearTimeout(timer); }
    const canvas = document.createElement('canvas');
    [canvas.width, canvas.height] = size(image.naturalWidth, image.naturalHeight);
    const ctx = canvas.getContext('2d');
    ctx.fillStyle = '#fff'; ctx.fillRect(0, 0, canvas.width, canvas.height);
    ctx.drawImage(image, 0, 0, canvas.width, canvas.height);
    const blob = await new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('Photo conversion timed out. Try a smaller photo.')), 15000);
      canvas.toBlob(b => { clearTimeout(timer); b ? resolve(b) : reject(new Error('Cannot convert this photo.')); }, 'image/jpeg', 0.9);
    });
    if (blob.size > MAX_BYTES) throw new Error('Converted photo exceeds 8 MiB. Choose a smaller photo.');
    const thumb = document.createElement('canvas');
    [thumb.width, thumb.height] = size(canvas.width, canvas.height, 160);
    thumb.getContext('2d').drawImage(canvas, 0, 0, thumb.width, thumb.height);
    return { blob, preview: thumb.toDataURL('image/jpeg', 0.8), name: file.name || 'Photo' };
  }
  const api = { MAX_COUNT, MAX_BYTES, validate, size, prepare };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckPhotos = api;
})(typeof window !== 'undefined' ? window : globalThis);
