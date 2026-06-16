import base64

from openai import OpenAI
from pathlib import Path

client = OpenAI(
    base_url="http://localhost:8787/openai/v1",
    api_key="sk-123"
)

def image_generation_example():
    response = client.images.generate(
        model="gemini-3-pro-image-preview",
        prompt="A cinematic cyberpunk rabbit wearing a yellow raincoat, neon city, high detail",
        n=1,
        size="1024x1024",
    )

    image = response.data[0]
    if image.url:
        print("Generated image URL:", image.url)
        return

    if image.b64_json:
        output_path = Path(__file__).with_name("generated_image.png")
        output_path.write_bytes(base64.b64decode(image.b64_json))
        print("Generated image saved to:", output_path)


if __name__ == "__main__":
    image_generation_example()
